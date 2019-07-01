/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"net/url"
	"strconv"

	"github.com/pkg/errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clientv1 "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset/typed/cluster/v1alpha1"
	clusterErr "sigs.k8s.io/cluster-api/pkg/controller/error"
	remotev1 "sigs.k8s.io/cluster-api/pkg/controller/remote"
	controllerClient "sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/actuators"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/constants"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/services/certificates"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/services/kubeclient"
)

// Actuator is responsible for maintaining the Cluster objects.
type Actuator struct {
	client           clientv1.ClusterV1alpha1Interface
	coreClient       corev1.CoreV1Interface
	controllerClient controllerClient.Client
}

// NewActuator returns a new instance of Actuator.
func NewActuator(
	client clientv1.ClusterV1alpha1Interface,
	coreClient corev1.CoreV1Interface,
	controllerClient controllerClient.Client) *Actuator {

	return &Actuator{
		client:           client,
		coreClient:       coreClient,
		controllerClient: controllerClient,
	}
}

// Reconcile will create or update the cluster
func (a *Actuator) Reconcile(cluster *clusterv1.Cluster) (opErr error) {
	ctx, err := context.NewClusterContext(&context.ClusterContextParams{
		Cluster:    cluster,
		Client:     a.client,
		CoreClient: a.coreClient,
		Logger:     klogr.New().WithName("[cluster-actuator]"),
	})
	if err != nil {
		return err
	}

	defer func() {
		opErr = actuators.PatchAndHandleError(ctx, "Reconcile", opErr)
	}()

	ctx.Logger.V(6).Info("reconciling cluster")

	if err := a.reconcilePKI(ctx); err != nil {
		return err
	}

	if err := a.reconcileReadyState(ctx); err != nil {
		return err
	}

	return nil
}

// Delete will delete any cluster level resources for the cluster.
func (a *Actuator) Delete(cluster *clusterv1.Cluster) (opErr error) {
	ctx, err := context.NewClusterContext(&context.ClusterContextParams{
		Cluster:    cluster,
		Client:     a.client,
		CoreClient: a.coreClient,
	})
	if err != nil {
		return err
	}

	defer func() {
		opErr = actuators.PatchAndHandleError(ctx, "Delete", opErr)
	}()

	ctx.Logger.V(2).Info("deleting cluster")

	// if deleteKubeconfig fails, return requeue error so kubeconfig
	// secret is properly cleaned up
	if err := a.deleteKubeConfig(ctx); err != nil {
		return errors.Wrapf(&clusterErr.RequeueAfterError{RequeueAfter: constants.DefaultRequeue},
			"error deleting kubeconfig secret for cluster %q", ctx)
	}

	return nil
}

// GetIP returns the control plane endpoint for the cluster.
func (a *Actuator) GetIP(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error) {
	clusterContext, err := context.NewClusterContext(&context.ClusterContextParams{
		Cluster:    cluster,
		Client:     a.client,
		CoreClient: a.coreClient,
	})
	if err != nil {
		return "", err
	}
	machineContext, err := context.NewMachineContextFromClusterContext(clusterContext, machine)
	if err != nil {
		return "", err
	}
	return machineContext.ControlPlaneEndpoint()
}

// GetKubeConfig returns the contents of a Kubernetes configuration file that
// may be used to access the cluster.
func (a *Actuator) GetKubeConfig(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error) {
	clusterContext, err := context.NewClusterContext(&context.ClusterContextParams{
		Cluster:    cluster,
		Client:     a.client,
		CoreClient: a.coreClient,
	})
	if err != nil {
		return "", err
	}

	if machine == nil {
		return kubeclient.GetKubeConfig(clusterContext)
	}

	machineContext, err := context.NewMachineContextFromClusterContext(clusterContext, machine)
	if err != nil {
		return "", err
	}

	return kubeclient.GetKubeConfig(machineContext)
}

func (a *Actuator) reconcilePKI(ctx *context.ClusterContext) error {
	if err := certificates.ReconcileCertificates(ctx); err != nil {
		return errors.Wrapf(err, "unable to reconcile certs while reconciling cluster %q", ctx)
	}
	return nil
}

func (a *Actuator) reconcileReadyState(ctx *context.ClusterContext) error {

	// Always remove the ready annotation. Ready state is determined
	// every time during reconciliation.
	delete(ctx.Cluster.Annotations, constants.ReadyAnnotationLabel)

	// Always recalculate the API Endpoints.
	ctx.Cluster.Status.APIEndpoints = []clusterv1.APIEndpoint{}

	// Reset the cluster's ready status
	ctx.ClusterStatus.Ready = false

	// List the target cluster's nodes to verify the target cluster is online.
	client, err := remotev1.NewClusterClient(a.controllerClient, ctx.Cluster)
	if err != nil {
		ctx.Logger.V(6).Info("unable to get client for target cluster", "reason", err.Error())
		return &clusterErr.RequeueAfterError{RequeueAfter: constants.DefaultRequeue}
	}
	coreClient, err := client.CoreV1()
	if err != nil {
		ctx.Logger.V(6).Info("unable to get core client for target cluster", "reason", err.Error())
		return &clusterErr.RequeueAfterError{RequeueAfter: constants.DefaultRequeue}
	}
	if _, err := coreClient.Nodes().List(metav1.ListOptions{}); err != nil {
		ctx.Logger.V(6).Info("unable to list nodes for target cluster", "reason", err.Error())
		return &clusterErr.RequeueAfterError{RequeueAfter: constants.DefaultRequeue}
	}

	restConfig := client.RESTConfig()
	if restConfig == nil {
		ctx.Logger.V(6).Info("unable to get rest config target cluster", "reason", err.Error())
		return &clusterErr.RequeueAfterError{RequeueAfter: constants.DefaultRequeue}
	}

	// Calculate the API endpoint for the cluster.
	controlPlaneEndpointURL, err := url.Parse(restConfig.Host)
	if err != nil {
		return errors.Wrapf(err, "unable to parse cluster's restConifg host value: %v", restConfig.Host)
	}

	// The API endpoint may just have a host.
	apiEndpoint := clusterv1.APIEndpoint{
		Host: controlPlaneEndpointURL.Hostname(),
	}

	// Check to see if there is also a port.
	if szPort := controlPlaneEndpointURL.Port(); szPort != "" {
		port, err := strconv.Atoi(szPort)
		if err != nil {
			return errors.Wrapf(err, "unable to get parse host and port for control plane endpoint %q for %q", controlPlaneEndpointURL.Host, ctx)
		}
		apiEndpoint.Port = port
	}

	// Update the API endpoints.
	ctx.Cluster.Status.APIEndpoints = []clusterv1.APIEndpoint{apiEndpoint}
	ctx.Logger.V(6).Info("calculated API endpoint for target cluster", "api-endpoint-host", apiEndpoint.Host, "api-endpoint-port", apiEndpoint.Port)

	// Update the kubeadm control plane endpoint with the one from the kubeconfig.
	if ctx.ClusterConfig.ClusterConfiguration.ControlPlaneEndpoint != controlPlaneEndpointURL.Host {
		ctx.ClusterConfig.ClusterConfiguration.ControlPlaneEndpoint = controlPlaneEndpointURL.Host
		ctx.Logger.V(6).Info("stored control plane endpoint in kubeadm cluster config", "control-plane-endpoint", controlPlaneEndpointURL.Host)
	}

	// Update the ready status.
	ctx.ClusterStatus.Ready = true

	// Update the ready annotation.
	if ctx.Cluster.Annotations == nil {
		ctx.Cluster.Annotations = map[string]string{}
	}
	ctx.Cluster.Annotations[constants.ReadyAnnotationLabel] = ""

	ctx.Logger.V(6).Info("cluster is ready")
	return nil
}

func (a *Actuator) deleteKubeConfig(ctx *context.ClusterContext) error {
	secretName := remotev1.KubeConfigSecretName(ctx.Cluster.Name)
	if err := a.coreClient.Secrets(ctx.Cluster.Namespace).Delete(secretName, &metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "error deleting kubeconfig secret for %q", ctx)
		}
	}
	return nil
}
