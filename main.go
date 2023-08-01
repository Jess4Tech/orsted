package main

import (
	"context"
	_ "embed"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	"helm.sh/helm/v3/pkg/repo"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	//go:embed values/cilium.yaml
	CiliumYaml string

	//go:embed values/rook-op.yaml
	RookOperatorYaml string

	//go:embed values/rook-cluster.yaml
	CephClusterYaml string

	//go:embed values/weave.yaml
	GitOpsYaml string
)

func main() {
	log.Println("We're in!")

	log.Println("Enabling and starting Kubelet and Cri-o")
	enableKubeletOut, err := RunCommand("bash", "-c", "systemctl enable --now kubelet crio")
	if err != nil {
		log.Printf("Systemctl output: %s\n", enableKubeletOut)
		log.Fatalf("Unable to enable kubelet and crio: %s\n", err)
	}

	log.Println("Kubelet and Cri-o started")

	log.Println("Initializing Kubernetes Cluster")
	kubeadmOut, err := RunCommand("kubeadm", "init", "--config", "/root/clusterconfig.yaml")
	if err != nil {
		log.Printf("Failed to run kubeadm: %s\n", err)
		log.Fatalf("Kubeadm output: %s\n", kubeadmOut)
	}

	k8sConf, err := clientcmd.BuildConfigFromFlags("", "/etc/kubernetes/admin.conf")
	if err != nil {
		log.Fatalf("Failed to parse kubernetes config: %s\n", err)
	}

	k8sClient, err := kubernetes.NewForConfig(k8sConf)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %s\n", err)
	}

	for true {
		pods, err := k8sClient.CoreV1().Pods("kube-system").List(context.Background(), meta.ListOptions{})
		if err != nil || len(pods.Items) == 0 {
			log.Printf("Kubernetes not yet ready: %s\n", err)
			time.Sleep(time.Second * 10)
			continue
		} else {
			log.Println("Kubernetes ready")
			break
		}
	}

	log.Println("Untainting node")
	clearTaintOut, err := RunCommand("bash", "-c", "kubectl taint nodes $(hostname -f) node-role.kubernetes.io/control-plane=master:NoSchedule- --kubeconfig='/etc/kubernetes/admin.conf'")
	if err != nil {
		log.Printf("Failed to clear master node taint: %s\n", err)
		log.Fatalf("Kubectl output: %s\n", clearTaintOut)
	}

	log.Println("Creating Gateway CRDs")
	// gatewayCRDsOut, err := RunCommand("bash", "-c", "curl -L https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml | kubectl apply --kubeconfig='/etc/kubernetes/admin.conf' -f -")
	gatewayCRDsOut, err := RunCommand("bash", "-c", "kubectl apply --kubeconfig='/etc/kubernetes/admin.conf' -f https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v0.7.1/config/crd/standard/gateway.networking.k8s.io_gatewayclasses.yaml -f https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v0.7.1/config/crd/standard/gateway.networking.k8s.io_gateways.yaml -f https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v0.7.1/config/crd/standard/gateway.networking.k8s.io_httproutes.yaml -f https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v0.7.1/config/crd/standard/gateway.networking.k8s.io_referencegrants.yaml -f https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v0.7.1/config/crd/experimental/gateway.networking.k8s.io_tlsroutes.yaml")
	if err != nil {
		log.Printf("Failed to apply gateway CRDs")
		log.Fatalf("Kubectl output: %s\n", gatewayCRDsOut)
	}

	log.Println("Adding Helm Repos")

	ciliumRepo := repo.Entry{
		Name: "cilium",
		URL:  "https://helm.cilium.io/",
	}

	helmClient, err := helmClientForNs("default")
	if err != nil {
		log.Fatalf("Failed to create helm client: %s\n", err)
	}

	if err = helmClient.AddOrUpdateChartRepo(ciliumRepo); err != nil {
		log.Fatalf("Failed to add Cilium Helm chart: %s\n", err)
	}

	kyvernoRepo := repo.Entry{
		Name: "kyverno",
		URL:  "https://kyverno.github.io/kyverno/",
	}

	if err = helmClient.AddOrUpdateChartRepo(kyvernoRepo); err != nil {
		log.Fatalf("Failed to add Kyverno Helm chart: %s\n", err)
	}

	rookRepo := repo.Entry{
		Name: "rook",
		URL:  "https://charts.rook.io/release",
	}

	if err = helmClient.AddOrUpdateChartRepo(rookRepo); err != nil {
		log.Fatalf("Failed to add Rook Ceph Helm chart: %s\n", err)
	}

	gitopsRepo := repo.Entry{
		Name: "gitops",
		URL:  "https://helm.gitops.weave.works/",
	}

	if err = helmClient.AddOrUpdateChartRepo(gitopsRepo); err != nil {
		log.Fatalf("Failed to add Weave GitOps Helm chart: %s\n", err)
	}

	defaultIp := GetDefaultIP().String()
	log.Printf("Default IP: %s\n", defaultIp)

	log.Println("Deploying Cilium")
	ciliumSpec := helmclient.ChartSpec{
		ReleaseName: "cilium",
		ChartName:   "cilium/cilium",
		Namespace:   "kube-system",
		UpgradeCRDs: true,
		Wait:        true,
		WaitForJobs: true,
		Timeout:     time.Minute * 7,
		Version:     "v1.14.0",
		ValuesYaml:  strings.Replace(CiliumYaml, "K8SHOST", defaultIp, 1),
	}

	if _, err := helmClient.InstallOrUpgradeChart(context.Background(), &ciliumSpec, nil); err != nil {
		log.Fatalf("Failed to install Cilium: %s\n", err)
	}

	log.Println("Creating Kyverno namespace")
	kyvNsSpec := core.Namespace{
		meta.TypeMeta{
			Kind:       "namespace",
			APIVersion: "v1",
		},
		meta.ObjectMeta{
			Name: "kyverno",
		},
		core.NamespaceSpec{},
		core.NamespaceStatus{},
	}
	_, err = k8sClient.CoreV1().Namespaces().Create(context.Background(), &kyvNsSpec, meta.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create kyverno namespace: %s\n", err)
	}

	kyvernoSpec := helmclient.ChartSpec{
		ReleaseName: "kyverno",
		ChartName:   "kyverno/kyverno",
		Namespace:   "kyverno",
		UpgradeCRDs: true,
		Wait:        true,
		WaitForJobs: true,
		Timeout:     time.Minute * 4,
	}

	log.Println("Deploying Kyverno")
	if err = InstallSpecWithNSClient("kyverno", &kyvernoSpec); err != nil {
		log.Fatalf("Failed to install Kyverno: %s\n", err)
	}

	rookNsSpec := core.Namespace{
		meta.TypeMeta{
			Kind:       "namespace",
			APIVersion: "v1",
		},
		meta.ObjectMeta{
			Name:   "rook-ceph",
			Labels: map[string]string{"pod-security.kubernetes.io/enforce": "privileged"},
		},
		core.NamespaceSpec{},
		core.NamespaceStatus{},
	}

	log.Println("Creating rook-ceph namespace")
	_, err = k8sClient.CoreV1().Namespaces().Create(context.Background(), &rookNsSpec, meta.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create rook-ceph namespace: %s\n", err)
	}

	rookOROut, err := RunCommand("bash", "-c", "kubectl apply --kubeconfig='/etc/kubernetes/admin.conf' -f /root/rook-overrides.yaml")
	if err != nil {
		log.Printf("Failed to create rook overrides: %s\n", err)
		log.Fatalf("Kubectl output: %s\n", rookOROut)
	}

	rookHelm, err := helmClientForNs("rook-ceph")
	if err != nil {
		log.Fatalf("Failed to create rook helm client")
	}

	rookOpSpec := helmclient.ChartSpec{
		ReleaseName: "rook-ceph",
		ChartName:   "rook/rook-ceph",
		Namespace:   "rook-ceph",
		Wait:        true,
		WaitForJobs: true,
		Timeout:     time.Minute * 2,
		UpgradeCRDs: true,
		ValuesYaml:  RookOperatorYaml,
	}

	log.Println("Deploying Rook Ceph operator")
	if _, err := rookHelm.InstallOrUpgradeChart(context.Background(), &rookOpSpec, nil); err != nil {
		log.Fatalf("Failed to install rook-ceph operator: %s\n", err)
	}

	rookClusterSpec := helmclient.ChartSpec{
		ReleaseName: "rook-ceph-cluster",
		ChartName:   "rook/rook-ceph-cluster",
		Namespace:   "rook-ceph",
		Wait:        true,
		WaitForJobs: true,
		Timeout:     time.Minute * 5,
		UpgradeCRDs: true,
		ValuesYaml:  CephClusterYaml,
	}

	log.Println("Deploying Rook Ceph cluster")
	if _, err := rookHelm.InstallOrUpgradeChart(context.Background(), &rookClusterSpec, nil); err != nil {
		log.Fatalf("Failed to install rook-ceph-cluster: %s\n", err)
	}

	gitopsNsSpec := core.Namespace{
		meta.TypeMeta{
			Kind:       "namespace",
			APIVersion: "v1",
		},
		meta.ObjectMeta{
			Name: "weave-gitops",
		},
		core.NamespaceSpec{},
		core.NamespaceStatus{},
	}

	log.Println("Creating weave-gitops namespace")
	_, err = k8sClient.CoreV1().Namespaces().Create(context.Background(), &gitopsNsSpec, meta.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create weave-gitops namespace: %s\n", err)
	}

	gitopsSpec := helmclient.ChartSpec{
		ReleaseName: "weave-gitops",
		ChartName:   "gitops/weave-gitops",
		Namespace:   "weave-gitops",
		Wait:        true,
		WaitForJobs: true,
		Timeout:     time.Minute * 15,
		ValuesYaml:  GitOpsYaml,
	}
	log.Println("Deploying Weave GitOps")
	if err = InstallSpecWithNSClient("weave-gitops", &gitopsSpec); err != nil {
		log.Fatalf("Failed to install weave-gitops: %s\n", err)
	}

	log.Println("Installing default policies")
	defPolOut, err := RunCommand("bash", "-c", "kubectl apply --kubeconfig='/etc/kubernetes/admin.conf' -f /root/default-policies.yaml")
	if err != nil {
		log.Printf("Failed to install default kyverno policies: %s\n", err)
		log.Fatalf("Kubectl output: %s\n", defPolOut)
	}
	log.Println("Successfully initialized Kubernetes Cluster")
}

var kubeConfig = []byte{}

func initKubeConf() {
	if len(kubeConfig) == 0 {
		kubeConfigI, err := os.ReadFile("/etc/kubernetes/admin.conf")
		if err != nil {
			log.Fatalf("Failed to read kubeconfig file: %s\n", err)
		}
		kubeConfig = kubeConfigI
	}
}

func helmClientForNs(ns string) (helmclient.Client, error) {
	initKubeConf()
	kubeConfOptions := helmclient.KubeConfClientOptions{
		Options: &helmclient.Options{
			Namespace:        ns,
			RepositoryCache:  "/tmp/.helmcache",
			RepositoryConfig: "/tmp/.helmrepo",
			Debug:            false,
			Linting:          true,
		},
		KubeContext: "",
		KubeConfig:  kubeConfig,
	}

	return helmclient.NewClientFromKubeConf(&kubeConfOptions)
}

func InstallSpecWithNSClient(ns string, spec *helmclient.ChartSpec) error {
	client, err := helmClientForNs(ns)
	if err != nil {
		return err
	}

	if _, err := client.InstallChart(context.Background(), spec, nil); err != nil {
		return err
	}

	return nil
}

func RunCommand(command string, args ...string) (string, error) {
	var out strings.Builder
	cmd := exec.Command(command, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func GetDefaultIP() net.IP {
	conn, err := net.Dial("udp", "1.1.1.1:80")
	if err != nil {
		log.Fatalf("Failed to get default ip: %s", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}
