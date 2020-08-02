package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	gkev1 "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

const kubeConfigGsaTemp = `
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %[1]s
    server: https://%[2]s
  name: %[3]s
contexts:
- context:
    cluster: %[3]s
    user: %[3]s
  name: %[3]s
current-context: %[3]s
kind: Config
preferences: {}
users:
- name: %[3]s
  user:
    auth-provider:
      config:
        cmd-args: get-credential
        cmd-path: %[4]s
        expiry-key: '{.token_expiry}'
        token-key: '{.access_token}'
      name: gcp`
const kubeConfigKsaTemp = `
      apiVersion: v1
      clusters:
      - cluster:
          certificate-authority-data: %[1]s
          server: https://%[2]s
        name: %[3]s
      contexts:
      - context:
          cluster: %[3]s
          user: %[4]s
        name: %[3]s
      current-context: %[3]s
      kind: Config
      preferences: {}
      users:
      - name: %[4]s
        user:
          token: %[5]s`

type gsaToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type,omitempty"`
}

type k8sCredential struct {
	AccessToken string `json:"access_token"`
	TokenExpiry string `json:"token_expiry"`
}

type vmInfo struct {
	InstanceName string
	Hostname     string
	InternalIP   string
	ExternalIP   string
	ProjectID    string
	ClusterName  string
	ClusterZone  string
	VMLabels     map[string]string
	KsaName      string
	KsaToken     string
}

func getCredentials() {
	jsonData, err := metadata.Get("instance/service-accounts/default/token")
	if err != nil {
		panic(err.Error())
	}

	token := gsaToken{}
	err = json.Unmarshal([]byte(jsonData), &token)
	if err != nil {
		panic(err.Error())
	}

	cred := k8sCredential{}
	cred.AccessToken = token.AccessToken
	expiryTime := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	cred.TokenExpiry = expiryTime.UTC().Format(time.RFC3339)
	ret, err := json.Marshal(cred)
	if err != nil {
		panic(err.Error())
	}
	fmt.Println(string(ret))
}

func getVMInfo() (ret vmInfo) {
	var err error

	ret.InstanceName, err = metadata.InstanceName()
	if err != nil {
		panic(err.Error())
	}
	ret.Hostname, err = metadata.Hostname()
	if err != nil {
		panic(err.Error())
	}
	ret.InternalIP, err = metadata.InternalIP()
	if err != nil {
		panic(err.Error())
	}
	ret.ExternalIP, err = metadata.ExternalIP()
	if err != nil {
		panic(err.Error())
	}
	ret.ProjectID, err = metadata.ProjectID()
	if err != nil {
		panic(err.Error())
	}

	ret.ClusterName, err = metadata.InstanceAttributeValue("k8s-cluster-name")
	if err != nil {
		panic(err.Error())
	}
	ret.ClusterZone, err = metadata.InstanceAttributeValue("k8s-cluster-zone")
	if err != nil {
		panic(err.Error())
	}

	ret.KsaName, err = metadata.InstanceAttributeValue("k8s-sa-name")
	if err != nil {
		ret.KsaName = ""
	}
	ret.KsaToken, err = metadata.InstanceAttributeValue("k8s-sa-token")
	if err != nil {
		ret.KsaToken = ""
	}

	ret.VMLabels = make(map[string]string)
	attrs, err := metadata.InstanceAttributes()
	if err != nil {
		panic(err.Error())
	}
	for _, name := range attrs {
		if strings.HasPrefix(name, "k8s-label-") {
			val, err := metadata.InstanceAttributeValue(name)
			if err != nil {
				klog.V(0).Infof("Faild to fetch label %s: %q", name, err)
			}
			ret.VMLabels[name[10:]] = val
		}
	}

	return
}

func getKubeConfig(vm vmInfo) (config *rest.Config) {
	// if homedir else get a fake kubectl
	var err error
	home := homedir.HomeDir()
	useConfigFile := false
	if home != "" {
		if _, err := os.Stat(filepath.Join(homedir.HomeDir(), ".kube", "config")); !os.IsNotExist(err) {
			useConfigFile = true
		}
	}
	if useConfigFile {
		config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config"))
		if err != nil {
			panic(err.Error())
		}
	} else {
		config = newKubeConfig(vm)
	}

	return
}

func newKubeConfig(vm vmInfo) (config *rest.Config) {
	// Get contianer master address and CA
	oauthClient, _, err := transport.NewHTTPClient(context.TODO(),
		option.WithScopes(gkev1.CloudPlatformScope))
	if err != nil {
		klog.Fatalf("failed to initalize http client: %+v", err)
	}
	gkeSvc, err := gkev1.New(oauthClient)
	if err != nil {
		klog.Fatalf("failed to initialize gke client: %+v", err)
	}
	clusterSvc := gkev1.NewProjectsZonesClustersService(gkeSvc)
	cluster, err := clusterSvc.Get(vm.ProjectID, vm.ClusterZone, vm.ClusterName).Do()
	if err != nil {
		klog.Fatalf("failed to get gke cluster: %+v", err)
	}

	kubeConfig := ""
	if vm.KsaName != "" && vm.KsaToken != "" {
		kubeConfig = fmt.Sprintf(kubeConfigKsaTemp, cluster.MasterAuth.ClusterCaCertificate, cluster.Endpoint,
			cluster.Name, vm.KsaName, vm.KsaToken)
	} else {
		pwd, err := os.Getwd()
		if err != nil {
			klog.Fatalf("failed to get current dir: %+v", err)
		}
		path := filepath.Join(pwd, os.Args[0])
		kubeConfig = fmt.Sprintf(kubeConfigGsaTemp, cluster.MasterAuth.ClusterCaCertificate, cluster.Endpoint,
			cluster.Name, path)
	}

	config, err = clientcmd.RESTConfigFromKubeConfig([]byte(kubeConfig))
	if err != nil {
		klog.Fatalf("failed to create kubeconfig: %+v", err)
	}

	return
}

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %v [command] \n", os.Args[0])
		return
	}
	switch os.Args[1] {
	case "get-credential":
		getCredentials()
		return
	case "start":
		vm := getVMInfo()
		config := getKubeConfig(vm)

		// create the clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err.Error())
		}

		podsClient := clientset.CoreV1().Pods(corev1.NamespaceDefault)
		podList, err := podsClient.List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}

		for _, item := range podList.Items {
			println(item.Name)
		}

		return
	default:
		fmt.Printf("Usage: %v [command] \n", os.Args[0])
		return
	}

	// Problem: if daemon, how to shutdown? User signal?
}
