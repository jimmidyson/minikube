/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/docker/machine/drivers/virtualbox"
	"github.com/docker/machine/libmachine"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/host"
	"github.com/docker/machine/libmachine/state"
	"github.com/golang/glog"
	"github.com/google/go-github/github"
	kubeApi "k8s.io/kubernetes/pkg/api"
	kubeUnversionedApi "k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/sshutil"
	"k8s.io/minikube/pkg/util"
)

var (
	certs = []string{"apiserver.crt", "apiserver.key"}
)

//This init function is used to set the logtostderr variable to false so that INFO level log info does not clutter the CLI
//INFO lvl logging is displayed due to the kubernetes api calling flag.Set("logtostderr", "true") in its init()
//see: https://github.com/kubernetes/kubernetes/blob/master/pkg/util/logs.go#L32-34
func init() {
	flag.Set("logtostderr", "false")
}

// StartHost starts a host VM.
func StartHost(api libmachine.API, config MachineConfig) (*host.Host, error) {
	if exists, err := api.Exists(constants.MachineName); err != nil {
		return nil, fmt.Errorf("Error checking if host exists: %s", err)
	} else if exists {
		glog.Infoln("Machine exists!")
		h, err := api.Load(constants.MachineName)
		if err != nil {
			return nil, fmt.Errorf("Error loading existing host: %s", err)
		}
		s, err := h.Driver.GetState()
		if err != nil {
			return nil, fmt.Errorf("Error getting state for host: %s", err)
		}
		if s != state.Running {
			if err := h.Driver.Start(); err != nil {
				return nil, fmt.Errorf("Error starting stopped host: %s", err)
			}
			if err := api.Save(h); err != nil {
				return nil, fmt.Errorf("Error saving started host: %s", err)
			}
		}
		if err := h.ConfigureAuth(); err != nil {
			return nil, fmt.Errorf("Error configuring auth on host: %s", err)
		}
		return h, nil
	} else {
		return createHost(api, config)
	}
}

// StopHost stops the host VM.
func StopHost(api libmachine.API) error {
	host, err := api.Load(constants.MachineName)
	if err != nil {
		return err
	}
	if err := host.Stop(); err != nil {
		return err
	}
	return nil
}

type multiError struct {
	Errors []error
}

func (m *multiError) Collect(err error) {
	if err != nil {
		m.Errors = append(m.Errors, err)
	}
}

func (m multiError) ToError() error {
	if len(m.Errors) == 0 {
		return nil
	}

	errStrings := []string{}
	for _, err := range m.Errors {
		errStrings = append(errStrings, err.Error())
	}
	return fmt.Errorf(strings.Join(errStrings, "\n"))
}

// DeleteHost deletes the host VM.
func DeleteHost(api libmachine.API) error {
	host, err := api.Load(constants.MachineName)
	if err != nil {
		return err
	}
	m := multiError{}
	m.Collect(host.Driver.Remove())
	m.Collect(api.Remove(constants.MachineName))
	return m.ToError()
}

// GetHostStatus gets the status of the host VM.
func GetHostStatus(api libmachine.API) (string, error) {
	dne := "Does Not Exist"
	exists, err := api.Exists(constants.MachineName)
	if err != nil {
		return "", err
	}
	if !exists {
		return dne, nil
	}

	host, err := api.Load(constants.MachineName)
	if err != nil {
		return "", err
	}

	s, err := host.Driver.GetState()
	if s.String() == "" {
		return dne, err
	}
	return s.String(), err
}

type sshAble interface {
	RunSSHCommand(string) (string, error)
}

// MachineConfig contains the parameters used to start a cluster.
type MachineConfig struct {
	MinikubeISO string
	Memory      int
	CPUs        int
	VMDriver    string
}

type KubernetesConfig struct {
	Version   string
	OpenShift bool
}

// StartCluster starts a k8s cluster on the specified Host.
func StartCluster(h sshAble, config KubernetesConfig) error {
	commands := []string{getStopCommand(config), GetStartCommand(config)}

	for _, cmd := range commands {
		glog.Infoln(cmd)
		output, err := h.RunSSHCommand(cmd)
		glog.Infoln(output)
		if err != nil {
			return err
		}
	}

	if config.OpenShift {
		for {
			cmd := "curl --fail -k https://localhost/healthz/ready"
			glog.Infoln(cmd)
			output, err := h.RunSSHCommand(cmd)
			glog.Infoln(output)
			if err == nil {
				break
			}
			<-time.After(time.Second * 1)
		}
		cmd := "sudo oc adm policy add-cluster-role-to-user cluster-admin admin --config=/var/lib/localkube/openshift.local.config/master/admin.kubeconfig"
		glog.Infoln(cmd)
		output, err := h.RunSSHCommand(cmd)
		glog.Infoln(output)
		if err != nil {
			return err
		}
	}

	return nil
}

func getStopCommand(config KubernetesConfig) string {
	if config.OpenShift {
		return "sudo killall openshift | true"
	}
	return stopCommand
}

type fileToCopy struct {
	AssetName   string
	TargetDir   string
	TargetName  string
	Permissions string
	Contents    []byte
	FileMode    os.FileMode
	LinkTarget  string
}

var deployAssets = []fileToCopy{
	{
		AssetName:   "deploy/iso/addon-manager.yaml",
		TargetDir:   "/etc/kubernetes/manifests/",
		TargetName:  "addon-manager.yaml",
		Permissions: "0640",
	},
	{
		AssetName:   "deploy/addons/dashboard-rc.yaml",
		TargetDir:   "/etc/kubernetes/addons/",
		TargetName:  "dashboard-rc.yaml",
		Permissions: "0640",
	},
	{
		AssetName:   "deploy/addons/dashboard-svc.yaml",
		TargetDir:   "/etc/kubernetes/addons/",
		TargetName:  "dashboard-svc.yaml",
		Permissions: "0640",
	},
}

var embeddedAssets = append(deployAssets,
	fileToCopy{
		AssetName:   "out/localkube",
		TargetDir:   "/usr/local/bin",
		TargetName:  "localkube",
		Permissions: "0777",
	},
)

func getAssets(config KubernetesConfig) ([]fileToCopy, error) {
	if config.OpenShift {
		return openshiftAssets(config)
	}

	if len(config.Version) > 0 {
		return kubernetesAssets(config)
	}

	return embeddedAssets, nil
}

func kubernetesAssets(config KubernetesConfig) ([]fileToCopy, error) {
	return embeddedAssets, nil
}

func openshiftAssets(config KubernetesConfig) ([]fileToCopy, error) {
	var (
		release *github.RepositoryRelease
		resp    *github.Response
		err     error
	)
	client := github.NewClient(nil)
	if len(config.Version) > 0 {
		release, resp, err = client.Repositories.GetReleaseByTag("openshift", "origin", "v"+config.Version)
	} else {
		release, resp, err = client.Repositories.GetLatestRelease("openshift", "origin")
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	assetID := getOpenShiftServerAssetID(release)
	if assetID == 0 {
		return nil, fmt.Errorf("Could not get OpenShift release URL")
	}
	asset, url, err := client.Repositories.DownloadReleaseAsset("openshift", "origin", assetID)
	if err != nil {
		return nil, fmt.Errorf("Could not download OpenShift release asset: %s", err)
	}
	if len(url) > 0 {
		httpResp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("Could not download OpenShift release asset: %s", err)
		}
		asset = httpResp.Body
	}

	defer asset.Close()

	var assets []fileToCopy
	gzf, err := gzip.NewReader(asset)
	if err != nil {
		return nil, fmt.Errorf("Could not ungzip OpenShift release asset: %s", err)
	}
	defer gzf.Close()
	tr := tar.NewReader(gzf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Could not extract OpenShift release asset: %s", err)
		}
		contents, err := ioutil.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("Could not extract OpenShift release asset: %s", err)
		}
		assetName := filepath.Base(hdr.Name)
		f := fileToCopy{
			AssetName:   assetName,
			TargetDir:   "/usr/local/bin",
			TargetName:  assetName,
			Permissions: "0777",
			Contents:    contents,
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			f.FileMode = os.ModeSymlink
			f.LinkTarget = filepath.Base(hdr.Linkname)
		}
		assets = append(assets, f)
	}
	return assets, nil
}

func getOpenShiftServerAssetID(release *github.RepositoryRelease) int {
	for _, asset := range release.Assets {
		if strings.HasPrefix(*asset.Name, "openshift-origin-server") && strings.HasSuffix(*asset.Name, "linux-64bit.tar.gz") {
			return *asset.ID
		}
	}
	return 0
}

func UpdateCluster(d drivers.Driver, config KubernetesConfig) error {
	client, err := sshutil.NewSSHClient(d)
	if err != nil {
		return err
	}

	assets, err := getAssets(config)
	if err != nil {
		return err
	}

	for _, a := range assets {
		switch a.FileMode {
		case os.ModeSymlink:
			if err := symlinkAsset(a, client); err != nil {
				return err
			}
		default:
			if err := transferAsset(a, client); err != nil {
				return err
			}
		}
	}
	return nil
}

func transferAsset(a fileToCopy, c *ssh.Client) error {
	contents := a.Contents
	if contents == nil {
		c, err := Asset(a.AssetName)
		if err != nil {
			glog.Infof("Error loading asset %s: %s", a.AssetName, err)
			return err
		}
		contents = c
	}

	glog.V(8).Infoln("Transferring", a.TargetName, "to", a.TargetDir)
	return sshutil.Transfer(contents, a.TargetDir, a.TargetName, a.Permissions, c)
}

func symlinkAsset(a fileToCopy, c *ssh.Client) error {
	symlinkName := filepath.Join(a.TargetDir, a.TargetName)
	symlinkTarget := filepath.Join(a.TargetDir, a.LinkTarget)
	cmd := fmt.Sprintf("sudo ln -s %s %s", symlinkTarget, symlinkName)
	return sshutil.RunCommand(c, cmd)
}

// SetupCerts gets the generated credentials required to talk to the APIServer.
func SetupCerts(d drivers.Driver) error {
	localPath := constants.Minipath
	ipStr, err := d.GetIP()
	if err != nil {
		return err
	}

	ip := net.ParseIP(ipStr)
	publicPath := filepath.Join(localPath, "apiserver.crt")
	privatePath := filepath.Join(localPath, "apiserver.key")
	if err := GenerateCerts(publicPath, privatePath, ip); err != nil {
		return err
	}

	client, err := sshutil.NewSSHClient(d)
	if err != nil {
		return err
	}

	for _, cert := range certs {
		p := filepath.Join(localPath, cert)
		data, err := ioutil.ReadFile(p)
		if err != nil {
			return err
		}
		if err := sshutil.Transfer(data, util.DefaultCertPath, cert, "0644", client); err != nil {
			return err
		}
	}
	return nil
}

func createHost(api libmachine.API, config MachineConfig) (*host.Host, error) {
	var driver interface{}

	switch config.VMDriver {
	case "virtualbox":
		d := virtualbox.NewDriver(constants.MachineName, constants.Minipath)
		d.Boot2DockerURL = config.MinikubeISO
		d.Memory = config.Memory
		d.CPU = config.CPUs
		driver = d
	case "vmwarefusion":
		driver = createVMwareFusionHost(config)
	case "kvm":
		driver = createKVMHost(config)
	case "xhyve":
		driver = createXhyveHost(config)
	default:
		glog.Exitf("Unsupported driver: %s\n", config.VMDriver)
	}

	data, err := json.Marshal(driver)
	if err != nil {
		return nil, err
	}

	h, err := api.NewHost(config.VMDriver, data)
	if err != nil {
		return nil, fmt.Errorf("Error creating new host: %s", err)
	}

	h.HostOptions.AuthOptions.CertDir = constants.Minipath
	h.HostOptions.AuthOptions.StorePath = constants.Minipath
	h.HostOptions.EngineOptions = &engine.Options{}

	if err := api.Create(h); err != nil {
		// Wait for all the logs to reach the client
		time.Sleep(2 * time.Second)
		return nil, fmt.Errorf("Error creating. %s", err)
	}

	if err := api.Save(h); err != nil {
		return nil, fmt.Errorf("Error attempting to save store: %s", err)
	}
	return h, nil
}

// GetHostDockerEnv gets the necessary docker env variables to allow the use of docker through minikube's vm
func GetHostDockerEnv(api libmachine.API) (map[string]string, error) {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return nil, err
	}
	ip, err := host.Driver.GetIP()
	if err != nil {
		return nil, err
	}

	tcpPrefix := "tcp://"
	portDelimiter := ":"
	port := "2376"

	envMap := map[string]string{
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_HOST":       tcpPrefix + ip + portDelimiter + port,
		"DOCKER_CERT_PATH":  constants.MakeMiniPath("certs"),
	}
	return envMap, nil
}

// GetHostLogs gets the localkube logs of the host VM.
func GetHostLogs(api libmachine.API) (string, error) {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return "", err
	}
	s, err := host.RunSSHCommand(logsCommand)
	if err != nil {
		return "", nil
	}
	return s, err
}

func checkIfApiExistsAndLoad(api libmachine.API) (*host.Host, error) {
	exists, err := api.Exists(constants.MachineName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("Machine does not exist for api.Exists(%s)", constants.MachineName)
	}

	host, err := api.Load(constants.MachineName)
	if err != nil {
		return nil, err
	}
	return host, nil
}

func CreateSSHShell(api libmachine.API, args []string) error {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return err
	}

	currentState, err := host.Driver.GetState()
	if err != nil {
		return err
	}

	if currentState != state.Running {
		return fmt.Errorf("Error: Cannot run ssh command: Host %q is not running", constants.MachineName)
	}

	client, err := host.CreateSSHClient()
	if err != nil {
		return err
	}
	return client.Shell(strings.Join(args, " "))
}

func GetDashboardURL(api libmachine.API) (string, error) {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return "", err
	}

	ip, err := host.Driver.GetIP()
	if err != nil {
		return "", err
	}

	client, err := getClient()
	if err != nil {
		return "", err
	}

	b, err := client.Get().AbsPath("/").DoRaw()
	if err != nil {
		return "", nil
	}

	var rootPaths kubeUnversionedApi.RootPaths
	if err := json.Unmarshal(b, &rootPaths); err != nil {
		return "", err
	}
	for _, rp := range rootPaths.Paths {
		if rp == "/oapi" {
			return fmt.Sprintf("https://%s", ip), nil
		}
	}

	port, err := getDashboardPort(client)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("http://%s:%d", ip, port), nil
}

type serviceGetter interface {
	Get(name string) (*kubeApi.Service, error)
}

func getDashboardPort(client *unversioned.Client) (int, error) {
	services, err := getKubernetesServicesWithNamespace(client, "kube-system")
	if err != nil {
		return 0, err
	}
	return getDashboardPortFromServiceGetter(services)
}

func getDashboardPortFromServiceGetter(services serviceGetter) (int, error) {
	dashboardService, err := services.Get("kubernetes-dashboard")
	if err != nil {
		return 0, fmt.Errorf("Error getting kubernetes-dashboard service: %s", err)
	}
	return int(dashboardService.Spec.Ports[0].NodePort), nil
}

func getKubernetesServicesWithNamespace(client *unversioned.Client, namespace string) (serviceGetter, error) {
	services := client.Services(namespace)
	return services, nil
}

func getClient() (*unversioned.Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("Error creating kubeConfig: %s", err)
	}
	client, err := unversioned.New(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}
