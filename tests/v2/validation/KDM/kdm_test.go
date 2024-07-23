package KDM

import (
	"context"
	"encoding/json"
	"fmt"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	stevev1 "github.com/rancher/shepherd/clients/rancher/v1"
	"github.com/rancher/shepherd/extensions/clusters"
	"github.com/rancher/shepherd/extensions/clusters/kubernetesversions"
	"github.com/rancher/shepherd/pkg/session"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"strings"
	"testing"
	"time"
)

const (
	rancherDeployment    = "rancher"
	rancherNamespace     = "cattle-system"
	rancherLabelSelector = "app=rancher"
	rkeMetadataConfig    = "rke-metadata-config"
)

var defaultBackoff = wait.Backoff{
	Duration: 1 * time.Second,
	Factor:   2.0,
	Steps:    7,
}

type KDMTestSuite struct {
	suite.Suite
	client    *rancher.Client
	session   *session.Session
	cluster   *management.Cluster
	config    *rest.Config
	clientset *kubernetes.Clientset
}

func (k *KDMTestSuite) SetupSuite() {
	k.session = session.NewSession()

	client, err := rancher.NewClient("", k.session)
	require.NoError(k.T(), err)

	k.client = client

	localCluster, err := k.client.Management.Cluster.ByID("local")
	k.Require().NoError(err)
	k.Require().NotEmpty(localCluster)
	localClusterKubeconfig, err := k.client.Management.Cluster.ActionGenerateKubeconfig(localCluster)
	k.Require().NoError(err)
	c, err := clientcmd.NewClientConfigFromBytes([]byte(localClusterKubeconfig.Config))
	k.Require().NoError(err)
	k.config, err = c.ClientConfig()
	k.Require().NoError(err)
	k.clientset, err = kubernetes.NewForConfig(k.config)
	k.Require().NoError(err)
}

func (k *KDMTestSuite) TearDownSuite() {
	k.session.Cleanup()
}

func (k *KDMTestSuite) updateKDMurl(value string) {
	// Use the Steve client instead of the main one to be able to set a setting's value to an empty string.
	existing, err := k.client.Steve.SteveType("management.cattle.io.setting").ByID(rkeMetadataConfig)
	k.Require().NoError(err, "error getting existing setting")

	var kdmSetting v3.Setting
	err = stevev1.ConvertToK8sType(existing.JSONResp, &kdmSetting)
	k.Require().NoError(err, "error converting existing setting")

	kdmData := map[string]string{}
	err = json.Unmarshal([]byte(kdmSetting.Value), &kdmData)
	k.Require().NoError(err, "error unmarshaling existing setting")

	kdmData["url"] = value
	val, err := json.Marshal(kdmData)
	k.Require().NoError(err, "error marshaling existing setting")
	kdmSetting.Value = string(val)
	_, err = k.client.Steve.SteveType("management.cattle.io.setting").Update(existing, kdmSetting)
	k.Require().NoError(err, "error updating setting")
}

func (k *KDMTestSuite) ScaleRancherTo(desiredReplicas int32) {
	// Get the current deployment
	deployment, err := k.clientset.AppsV1().Deployments(rancherNamespace).Get(context.TODO(), rancherDeployment, metav1.GetOptions{})
	k.Require().NoError(err, "error getting rancher deployment")

	// Scale the deployment to desired replicas
	if deployment.Spec.Replicas == &desiredReplicas {
		return
	}
	deployment.Spec.Replicas = &desiredReplicas

	// Update the deployment with the new replica count
	deployment, err = k.clientset.AppsV1().Deployments(rancherNamespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
	k.Require().NoError(err, "error updating rancher deployment")

	// Wait for the deployment to scale up using exponential defaultBackoff
	err = wait.ExponentialBackoff(defaultBackoff, func() (bool, error) {
		// Get the updated deployment
		deployment, err = k.clientset.AppsV1().Deployments(rancherNamespace).Get(context.TODO(), rancherDeployment, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("Error getting deployment: %s", err.Error())
		}

		// Check if the deployment has the desired number of replicas
		if deployment.Status.ReadyReplicas == desiredReplicas {
			fmt.Printf("Deployment %s successfully scaled to %d replicas\n", rancherDeployment, desiredReplicas)
			return true, nil
		}
		fmt.Printf("Waiting for deployment %s to scale. Current replicas: %d/%d\n", rancherDeployment, deployment.Status.ReadyReplicas, desiredReplicas)
		return false, nil
	})
	k.Require().NoError(err, "error scaling rancher deployment, timed out")
}

func (k *KDMTestSuite) GetRancherReplicas() *v1.PodList {
	podList, err := k.clientset.CoreV1().Pods(rancherNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: rancherLabelSelector})
	k.Require().NoError(err, "error getting rancher pod list")
	return podList
}

func (k *KDMTestSuite) ExecCMDForKDMDump(pod v1.Pod, cmd []string) string {
	req := k.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")
	req.VersionedParams(&v1.PodExecOptions{
		Container: "rancher",
		Command:   cmd,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, runtime.NewParameterCodec(runtime.NewScheme()))

	exec, err := remotecommand.NewSPDYExecutor(k.config, "POST", req.URL())
	k.Require().NoError(err, "error executing remote command")

	var stdout, stderr strings.Builder
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	k.Require().NoError(err, "error executing exec stream")

	return stdout.String()
}

func (k *KDMTestSuite) TestChangeKDMurl() {
	// change kdm url to dev
	k.updateKDMurl("https://releases.rancher.com/kontainer-driver-metadata/dev-v2.8/data.json")
	// scale Rancher to 3 replicas
	k.ScaleRancherTo(3)
	// get the current release value
	currentLatestRKE2Version, err := kubernetesversions.Default(k.client, clusters.RKE2ClusterType.String(), []string{})
	k.Require().NoError(err, "error getting kubernetes version")
	// change kdm url to release
	k.updateKDMurl("https://releases.rancher.com/kontainer-driver-metadata/release-v2.8/data.json")

	var updatedRKE2Version []string
	// check latest Release value
	err = wait.ExponentialBackoff(defaultBackoff, func() (bool, error) {
		updatedRKE2Version, err = kubernetesversions.Default(k.client, clusters.RKE2ClusterType.String(), []string{})
		if err != nil {
			return false, fmt.Errorf("error getting kubernetes version: %s", err.Error())
		}
		if updatedRKE2Version[0] != currentLatestRKE2Version[0] {
			// change detected
			return true, nil
		}
		return false, nil
	})
	if updatedRKE2Version[0] != currentLatestRKE2Version[0] {
		// look for updated version in all Rancher Pod

		// Command to execute in the pods
		cmd := []string{"curl", "--insecure", "https://0.0.0.0/v1-rke2-release/releases"}
		pods := k.GetRancherReplicas()
		for _, pod := range pods.Items {
			fmt.Println(pod.Name)
			output := k.ExecCMDForKDMDump(pod, cmd)
			if !strings.Contains(output, updatedRKE2Version[0]) {
				k.Require().Error(fmt.Errorf("found KDM from a pod:%v not matching with the latest known version:%v", pod.Name, updatedRKE2Version[0]))
			}
		}
	} else {
		// This is the scenario where both release and dev version of KDM have same latest version
		fmt.Println("nothing to assert here")
	}
}

func TestKDM(t *testing.T) {
	suite.Run(t, new(KDMTestSuite))
}
