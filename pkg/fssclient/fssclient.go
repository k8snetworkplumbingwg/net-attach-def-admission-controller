package fssclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"k8s.io/klog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type AuthOpts struct {
	AuthURL     string `gcfg:"auth-url" mapstructure:"auth-url"`
	Username    string
	Password    string
	Clustername string `gcfg:"cluster-name"`
	Restartmode string `gcfg:"restart-mode"`
}

type FssClient struct {
	cfg                AuthOpts
	rootURL            string
	refreshURL         string
	accessTokenExpiry  time.Time
	refreshTokenExpiry time.Time
	loginResponse      LoginResponse
	k8sClientSet       kubernetes.Interface
	podNamespace       string
	configmap          *corev1.ConfigMap
	plugin             Plugin
	deployment         Deployment
	database           Database
}

const (
	pluginPath              = "/rest/connect/api/v1/plugins/plugins"
	deploymentPath          = "/rest/connect/api/v1/plugins/deployments"
	tenantPath              = "/rest/connect/api/v1/plugins/tenants"
	subnetPath              = "/rest/connect/api/v1/plugins/subnets"
	hostPortLabelPath       = "/rest/connect/api/v1/plugins/hostportlabels"
	hostPortPath            = "/rest/connect/api/v1/plugins/hostports"
	hostPortAssociationPath = "/rest/connect/api/v1/plugins/hostportlabelhostportassociations"
	subnetAssociationPath   = "/rest/connect/api/v1/plugins/hostportlabelsubnetassociations"
)

func (f *FssClient) GetAccessToken() error {
	now := time.Now()
	// Check if accessToken expiried
	if now.After(f.accessTokenExpiry) {
		klog.Info("access_token expired, refresh it")
		return f.login(f.refreshURL)
	}
	// Check if refreshToken expiried
	if now.After(f.refreshTokenExpiry) {
		klog.Info("refresh_token expired, login again")
		return f.login(f.cfg.AuthURL)
	}
	return nil
}

func (f *FssClient) GET(path string) (int, []byte, error) {
	err := f.GetAccessToken()
	if err != nil {
		return 0, nil, err
	}
	u := f.rootURL + path
	request, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Add("Authorization", "Bearer "+f.loginResponse.AccessToken)
	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	jsonRespData, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, jsonRespData, err
}

func (f *FssClient) DELETE(path string) (int, []byte, error) {
	err := f.GetAccessToken()
	if err != nil {
		return 0, nil, err
	}
	u := f.rootURL + path
	request, err := http.NewRequest("DELETE", u, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Add("Authorization", "Bearer "+f.loginResponse.AccessToken)
	client := &http.Client{}
	response, err := client.Do(request)
	defer response.Body.Close()
	jsonRespData, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, jsonRespData, err
}

func (f *FssClient) POST(path string, jsonReqData []byte) (int, []byte, error) {
	err := f.GetAccessToken()
	if err != nil {
		return 0, nil, err
	}
	u := f.rootURL + path
	var jsonBody *bytes.Buffer
	if len(jsonReqData) > 0 {
		jsonBody = bytes.NewBuffer(jsonReqData)
	}
	request, err := http.NewRequest("POST", u, jsonBody)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	request.Header.Add("Authorization", "Bearer "+f.loginResponse.AccessToken)
	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	jsonRespData, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, jsonRespData, err
}

func (f *FssClient) getConfigMap(name string) []byte {
	return []byte(f.configmap.Data[name])
}

func (f *FssClient) setConfigMap(name string, data []byte) error {
	klog.V(3).Infof("Save %s to configMap fss-database", name)
	var err error
	for i := 0; i < 256; i++ {
		klog.V(3).Infof("Attempt %d", i+1)
		f.configmap, err = f.k8sClientSet.CoreV1().ConfigMaps(f.podNamespace).Get(context.TODO(), "fss-database", metav1.GetOptions{})
		f.configmap.Data[name] = string(data)
		_, err = f.k8sClientSet.CoreV1().ConfigMaps(f.podNamespace).Update(context.TODO(), f.configmap, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
	}
	return err
}

func (f *FssClient) TxnDone() error {
	jsonString := f.database.encode()
	err := f.setConfigMap("database", jsonString)
	return err
}

func (f *FssClient) login(loginURL string) error {
	var jsonReqData []byte
	if loginURL == f.refreshURL {
		jsonReqData, _ = json.Marshal(map[string]string{
			"refresh_token": f.loginResponse.RefreshToken,
		})
	} else {
		jsonReqData, _ = json.Marshal(map[string]string{
			"username": f.cfg.Username,
			"password": f.cfg.Password,
		})
	}
	request, err := http.NewRequest("POST", loginURL, bytes.NewBuffer(jsonReqData))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	if loginURL == f.refreshURL {
		request.Header.Add("Authorization", "Bearer "+f.loginResponse.AccessToken)
	}
	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("Login failed with code %d", response.StatusCode)
	}
	defer response.Body.Close()
	jsonRespData, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}
	var result LoginResponse
	err = json.Unmarshal(jsonRespData, &result)
	if err != nil {
		return err
	}
	now := time.Now()
	f.accessTokenExpiry = now.Add(time.Duration(result.ExpiresIn) * time.Second)
	if loginURL != f.refreshURL {
		f.refreshTokenExpiry = now.Add(time.Duration(result.RefreshExpiresIn) * time.Second)
	}
	f.loginResponse = result
	return nil
}

func NewFssClient(k8sClientSet kubernetes.Interface, podNamespace string, cfg *AuthOpts) (*FssClient, error) {
	u, err := url.Parse(cfg.AuthURL)
	if err != nil {
		return nil, err
	}
	f := &FssClient{
		cfg:          *cfg,
		rootURL:      u.Scheme + "://" + u.Host,
		refreshURL:   strings.Replace(cfg.AuthURL, "login", "refresh", 1),
		k8sClientSet: k8sClientSet,
		podNamespace: podNamespace,
	}
	// Login
	klog.Infof("Login to FSS: %s", cfg.AuthURL)
	err = f.login(cfg.AuthURL)
	if err != nil {
		return nil, err
	}
	// Check if this is the first run
	firstRun := false
	hasDeployment := false
	f.configmap, err = k8sClientSet.CoreV1().ConfigMaps(podNamespace).Get(context.TODO(), "fss-database", metav1.GetOptions{})
	if err != nil {
		firstRun = true
		klog.Infof("Create ConfigMap fss-database")
		f.configmap = &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				Kind:       "ConfigMap",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fss-database",
				Namespace: podNamespace,
			},
			Data: map[string]string{
				"plugin":     "",
				"deployment": "",
				"database":   "",
			},
		}
		f.configmap, err = f.k8sClientSet.CoreV1().ConfigMaps(podNamespace).Create(context.TODO(), f.configmap, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		klog.Infof("ConfigMap fss-database created")
	}
	// Check the last registration
	if !firstRun {
		klog.Infof("Check if last plugin is still valid")
		var plugin Plugin
		jsonString := f.getConfigMap("plugin")
		err = json.Unmarshal(jsonString, &plugin)
		if err == nil && len(plugin.ID) > 0 {
			klog.Infof("Plugin from last run: %+v", plugin)
			// Validate with Connect Core
			u := pluginPath + "/" + plugin.ID
			statusCode, _, err := f.GET(u)
			if err != nil || statusCode != 200 {
				klog.Infof("Plugin is not longer valid")
				firstRun = true
			} else {
				f.plugin = plugin
			}
		} else {
			klog.Infof("No plugin found from last run")
			firstRun = true
		}
	}
	// Check the last deployment
	if !firstRun {
		klog.Infof("Check if last deployment is still valid")
		var deployment Deployment
		jsonString := f.getConfigMap("deployment")
		if len(jsonString) > 0 {
			err = json.Unmarshal(jsonString, &deployment)
			if err == nil && deployment.PluginID == f.plugin.ID {
				klog.Infof("Deployment from last run: %+v", deployment)
				// Validate with Connect Core
				u := deploymentPath + "/" + deployment.ID
				statusCode, _, err := f.GET(u)
				if err != nil || statusCode != 200 {
					klog.Infof("Deployment is not longer valid")
				} else {
					hasDeployment = true
					f.deployment = deployment
				}
			} else {
				klog.Infof("No deployment found from last run")
			}
		}
	}
	if firstRun {
		klog.Infof("Start a new run")
		// Create plugin
		f.plugin = Plugin{
			ConnectType:            "kubernetes",
			Name:                   "ncs-" + cfg.Clustername,
			SupportsNewDeployments: true,
			ExternalID:             "ncs-" + cfg.Clustername,
		}
		jsonRequest, _ := json.Marshal(f.plugin)
		statusCode, jsonResponse, err := f.POST(pluginPath, jsonRequest)
		if err != nil {
			return nil, err
		}
		if statusCode != 201 {
			var errorResponse ErrorResponse
			json.Unmarshal(jsonResponse, &errorResponse)
			klog.Errorf("Plugin error: %+v", errorResponse)
			return nil, fmt.Errorf("Create plugin failed with code %d", statusCode)
		}
		json.Unmarshal(jsonResponse, &f.plugin)
		klog.Infof("Plugin created: %+v", f.plugin)
		jsonString, _ := json.Marshal(f.plugin)
		err = f.setConfigMap("plugin", jsonString)
		if err != nil {
			return nil, err
		}
		if !hasDeployment {
			f.deployment = Deployment{
				AdminUp:    false,
				Name:       "ncs-" + cfg.Clustername,
				PluginID:   f.plugin.ID,
				ExternalID: "ncs-" + cfg.Clustername,
			}
			/*
				// Create deployment is allowed with admin API only
				jsonRequest, _ = json.Marshal(f.deployment)
				statusCode, jsonResponse, err = f.POST(deploymentPath, jsonRequest)
				if err != nil {
					return nil, err
				}
				if statusCode != 201 {
					var errorResponse ErrorResponse
					json.Unmarshal(jsonResponse, &errorResponse)
					klog.Errorf("Deployment error: %+v", errorResponse)
					return nil, fmt.Errorf("Create deployment failed with code %d", statusCode)
				}
				json.Unmarshal(jsonResponse, &f.deployment)
				klog.Infof("Deployment created: %+v", f.deployment)
				jsonString, _ = json.Marshal(f.deployment)
				err = f.setConfigMap("deployment", jsonString)
				if err != nil {
					return nil, err
				}
			*/
		}
	}
	// Wait Admin to create deployment
	if !f.deployment.AdminUp {
		klog.Infof("Wait Admin to create deployment for plugin %s...", f.plugin.ID)
		for !f.deployment.AdminUp {
			time.Sleep(10 * time.Second)
			statusCode, jsonResponse, err := f.GET(deploymentPath)
			if err != nil || statusCode != 200 {
				return nil, fmt.Errorf("Get deployments failed: %s", err.Error())
			}
			var deployments Deployments
			json.Unmarshal(jsonResponse, &deployments)
			for _, v := range deployments {
				if v.PluginID == f.plugin.ID {
					klog.Infof("Deployment is create: %+v", v)
					f.deployment = v
					jsonString, _ := json.Marshal(f.deployment)
					err = f.setConfigMap("deployment", jsonString)
					if err != nil {
						return nil, err
					}
					break
				}
			}
		}
	}
	// Create database
	f.database = Database{
		tenants:        make(map[string]Tenant),
		subnets:        make(map[string]Subnet),
		hostPortLabels: make(map[string]HostPortLabelIDByVlan),
		attachedLabels: make(map[string]HostPortLabelIDByVlan),
		hostPorts:      make(map[string]HostPortIDByName),
		attachedPorts:  make(map[string][]HostPortAssociationIDByPort),
	}
	if firstRun {
		f.TxnDone()
	} else {
		if cfg.Restartmode == "cache" {
			klog.Infof("Load tenant data from last run")
			var database Database
			jsonString := f.getConfigMap("database")
			if len(jsonString) > 0 {
				database, err = database.decode(jsonString)
				if err == nil {
					f.database = database
				}
			}
		} else {
			klog.Infof("Fetch tenant data from server")
			err = f.Resync(f.deployment.ID)
			if err != nil {
				return nil, fmt.Errorf("Resync with server failed: %s", err.Error())
			}
			f.TxnDone()
		}
	}
	return f, nil
}

func (f *FssClient) Resync(deploymentID string) error {
	// Get and save tenants
	statusCode, jsonResponse, err := f.GET(tenantPath)
	if err != nil || statusCode != 200 {
		klog.Errorf("Get tenants failed: %s", err.Error())
		return err
	}
	var tenants Tenants
	json.Unmarshal(jsonResponse, &tenants)
	for _, v := range tenants {
		if v.DeploymentID == deploymentID {
			f.database.tenants[v.FssWorkloadEvpnID] = v
		}
	}
	// Get and save subnets
	statusCode, jsonResponse, err = f.GET(subnetPath)
	if err != nil || statusCode != 200 {
		klog.Errorf("Get subnets failed: %s", err.Error())
		return err
	}
	var subnets Subnets
	json.Unmarshal(jsonResponse, &subnets)
	for _, v := range subnets {
		if v.DeploymentID == deploymentID {
			f.database.subnets[v.FssSubnetID] = v
			f.database.hostPortLabels[v.FssSubnetID] = make(HostPortLabelIDByVlan)
			f.database.attachedLabels[v.FssSubnetID] = make(HostPortLabelIDByVlan)
		}
	}
	// Get and save hostPortLabels
	statusCode, jsonResponse, err = f.GET(hostPortLabelPath)
	if err != nil || statusCode != 200 {
		klog.Errorf("Get hostPortLabels failed: %s", err.Error())
		return err
	}
	var hostPortLabels HostPortLabels
	json.Unmarshal(jsonResponse, &hostPortLabels)
	for _, v := range hostPortLabels {
		if v.DeploymentID != deploymentID {
			continue
		}
		names := strings.Split(v.Name, "-")
		if len(names) != 3 || names[0] != "label" {
			continue
		}
		fssSubnetID := names[1]
		if _, ok := f.database.subnets[fssSubnetID]; !ok {
			continue
		}
		vlanID, err := strconv.Atoi(names[2])
		if err == nil {
			f.database.hostPortLabels[fssSubnetID][vlanID] = v.ID
		}
	}
	// Get and save hostPorts
	statusCode, jsonResponse, err = f.GET(hostPortPath)
	if err != nil || statusCode != 200 {
		klog.Errorf("Get hostPorts failed: %s", err.Error())
		return err
	}
	var hostPorts HostPorts
	json.Unmarshal(jsonResponse, &hostPorts)
	for _, v := range hostPorts {
		if v.DeploymentID == deploymentID {
			if _, ok := f.database.hostPorts[v.HostName]; !ok {
				f.database.hostPorts[v.HostName] = make(HostPortIDByName)
			}
			f.database.hostPorts[v.HostName][v.PortName] = v.ID
		}
	}
	// Get and save hostPortAssociations
	statusCode, jsonResponse, err = f.GET(hostPortAssociationPath)
	if err != nil || statusCode != 200 {
		klog.Errorf("Get hostPortAssociations failed: %s", err.Error())
		return err
	}
	var hostPortAssociations HostPortAssociations
	json.Unmarshal(jsonResponse, &hostPortAssociations)
	for _, v := range hostPortAssociations {
		if v.DeploymentID == deploymentID {
			portAssociation := make(HostPortAssociationIDByPort)
			portAssociation[v.HostPortID] = v.ID
			f.database.attachedPorts[v.HostPortLabelID] = append(f.database.attachedPorts[v.HostPortLabelID], portAssociation)
		}
	}
	// Get subnetAssociations
	statusCode, jsonResponse, err = f.GET(subnetAssociationPath)
	if err != nil || statusCode != 200 {
		klog.Errorf("Get subnetAssociations failed: %s", err.Error())
		return err
	}
	var subnetAssociations SubnetAssociations
	json.Unmarshal(jsonResponse, &subnetAssociations)
	for _, v := range subnetAssociations {
		if v.DeploymentID != deploymentID {
			continue
		}
		for k2, v2 := range f.database.hostPortLabels {
			if v2[v.VlanID] == v.HostPortLabelID {
				f.database.attachedLabels[k2][v.VlanID] = v.HostPortLabelID
			}
		}
	}
	return nil
}

func (f *FssClient) GetSubnetInterface(fssWorkloadEvpnId string, fssSubnetId string, vlanId int) (string, error) {
	hostPortLabelID := ""
	tenant, ok1 := f.database.tenants[fssWorkloadEvpnId]
	if !ok1 {
		// Create the tenant
		klog.Infof("Create tenant for fssWorkloadEvpnId %s", fssWorkloadEvpnId)
		tenant = Tenant{
			DeploymentID:      f.deployment.ID,
			FssWorkloadEvpnID: fssWorkloadEvpnId,
			Name:              "tenant-" + fssWorkloadEvpnId,
			FssManaged:        true,
		}
		jsonRequest, _ := json.Marshal(tenant)
		statusCode, jsonResponse, err := f.POST(tenantPath, jsonRequest)
		if err != nil {
			return hostPortLabelID, err
		}
		if statusCode != 201 {
			var errorResponse ErrorResponse
			json.Unmarshal(jsonResponse, &errorResponse)
			klog.Errorf("Tenant error: %+v", errorResponse)
			return hostPortLabelID, fmt.Errorf("Create tenant failed with code %d", statusCode)
		}
		json.Unmarshal(jsonResponse, &tenant)
		klog.Infof("Tenant is created: %+v", tenant)
		f.database.tenants[fssWorkloadEvpnId] = tenant
	}
	subnet, ok2 := f.database.subnets[fssSubnetId]
	if !ok2 {
		// Create the subnet
		klog.Infof("Create subnet for fssSubnetId %s", fssSubnetId)
		subnet = Subnet{
			DeploymentID: f.deployment.ID,
			TenantID:     f.database.tenants[fssWorkloadEvpnId].ID,
			FssSubnetID:  fssSubnetId,
			Name:         "subnet-" + fssSubnetId,
			FssManaged:   true,
		}
		jsonRequest, _ := json.Marshal(subnet)
		statusCode, jsonResponse, err := f.POST(subnetPath, jsonRequest)
		if err != nil {
			return hostPortLabelID, err
		}
		if statusCode != 201 {
			var errorResponse ErrorResponse
			json.Unmarshal(jsonResponse, &errorResponse)
			klog.Errorf("Subnet error: %+v", errorResponse)
			return hostPortLabelID, fmt.Errorf("Create subnet failed with code %d", statusCode)
		}
		json.Unmarshal(jsonResponse, &subnet)
		klog.Infof("Subnet is created: %+v", subnet)
		f.database.subnets[fssSubnetId] = subnet
		f.database.hostPortLabels[fssSubnetId] = make(HostPortLabelIDByVlan)
		f.database.attachedLabels[fssSubnetId] = make(HostPortLabelIDByVlan)
	}
	hostPortLabels := f.database.hostPortLabels[fssSubnetId]
	hostPortLabelID, ok3 := hostPortLabels[vlanId]
	if ok1 && ok2 && ok3 {
		return hostPortLabelID, nil
	}
	// Create the hostPortLabel
	klog.Infof("Create hostPortLabel for fssSubnetId %s and vlanId %d", fssSubnetId, vlanId)
	hostPortLabel := HostPortLabel{
		DeploymentID: f.deployment.ID,
		Name:         "label-" + fssSubnetId + "-" + strconv.Itoa(vlanId),
	}
	jsonRequest, _ := json.Marshal(hostPortLabel)
	statusCode, jsonResponse, err := f.POST(hostPortLabelPath, jsonRequest)
	if err != nil {
		return hostPortLabelID, err
	}
	if statusCode != 201 {
		var errorResponse ErrorResponse
		json.Unmarshal(jsonResponse, &errorResponse)
		klog.Errorf("HostPortLabel error: %+v", errorResponse)
		return hostPortLabelID, fmt.Errorf("Create hostPortLabel failed with code %d", statusCode)
	}
	json.Unmarshal(jsonResponse, &hostPortLabel)
	klog.Infof("HostPortLabel is created: %+v", hostPortLabel)
	f.database.hostPortLabels[fssSubnetId][vlanId] = hostPortLabel.ID
	return hostPortLabel.ID, nil
}

func (f *FssClient) AttachSubnetInterface(fssSubnetId string, vlanId int, hostPortLabelID string) error {
	klog.Infof("Attach hostPortLabel %s to fssSubnetId %s for vlanId %d", hostPortLabelID, fssSubnetId, vlanId)
	attachedLabels := f.database.attachedLabels[fssSubnetId]
	_, ok := attachedLabels[vlanId]
	if ok && hostPortLabelID == attachedLabels[vlanId] {
		klog.Infof("hostPortLabel %s already attached", hostPortLabelID)
		return nil
	}
	subnetAssociation := SubnetAssociation{
		DeploymentID:    f.deployment.ID,
		HostPortLabelID: hostPortLabelID,
		SubnetID:        f.database.subnets[fssSubnetId].ID,
		VlanID:          vlanId,
	}
	jsonRequest, _ := json.Marshal(subnetAssociation)
	statusCode, jsonResponse, err := f.POST(subnetAssociationPath, jsonRequest)
	if err != nil {
		return err
	}
	if statusCode != 201 {
		var errorResponse ErrorResponse
		json.Unmarshal(jsonResponse, &errorResponse)
		klog.Errorf("SubnetAssociation error: %+v", errorResponse)
		return fmt.Errorf("Create SubnetAssociation failed with code %d", statusCode)
	}
	json.Unmarshal(jsonResponse, &subnetAssociation)
	klog.Infof("SubnetAssociation is created: %+v", subnetAssociation)
	f.database.attachedLabels[fssSubnetId][vlanId] = subnetAssociation.HostPortLabelID
	return nil
}

func (f *FssClient) DeleteSubnetInterface(fssSubnetId string, vlanId int, hostPortLabelID string) error {
	klog.Infof("Delete hostPortLabel %s for fssSubnetId %s and vlanId %d", hostPortLabelID, fssSubnetId, vlanId)
	_, ok := f.database.attachedLabels[fssSubnetId][vlanId]
	if ok && hostPortLabelID == f.database.attachedLabels[fssSubnetId][vlanId] {
		// HostPortLabel: When deleting a HostPortLabel, the associations to Subnet and HostPort are automatically deleted.
		u := hostPortLabelPath + "/" + hostPortLabelID
		statusCode, _, err := f.DELETE(u)
		if err != nil {
			return err
		}
		if statusCode != 204 {
			return fmt.Errorf("Delete hostPortLabel failed with code %d", statusCode)
		}
		klog.Infof("HostPortLabel %s is deleted", hostPortLabelID)
	} else {
		klog.Infof("HostPortLabel %s does not exists", hostPortLabelID)
	}
	// Local deletion: hostPortLabels, attacheLabels, attachedHostPorts
	delete(f.database.hostPortLabels[fssSubnetId], vlanId)
	delete(f.database.attachedLabels[fssSubnetId], vlanId)
	delete(f.database.attachedPorts, hostPortLabelID)
	return nil
}

func (f *FssClient) AttachHostPort(hostPortLabelID string, node string, port string, nodeCreated bool) error {
	hostPorts, ok := f.database.hostPorts[node]
	if !ok {
		f.database.hostPorts[node] = make(HostPortIDByName)
		hostPorts = f.database.hostPorts[node]
	}
	// Check if port exists
	hostPortID, ok := hostPorts[port]
	if !ok {
		klog.Infof("Create hostPort for host %s port %s", node, port)
		hostPort := HostPort{
			DeploymentID: f.deployment.ID,
			HostName:     node,
			PortName:     port,
		}
		jsonRequest, _ := json.Marshal(hostPort)
		statusCode, jsonResponse, err := f.POST(hostPortPath, jsonRequest)
		if err != nil {
			return err
		}
		if statusCode != 201 {
			var errorResponse ErrorResponse
			json.Unmarshal(jsonResponse, &errorResponse)
			klog.Errorf("HostPort error: %+v", errorResponse)
			return fmt.Errorf("Create hostPort failed with code %d", statusCode)
		}
		json.Unmarshal(jsonResponse, &hostPort)
		klog.Infof("HostPort is created: %+v", hostPort)
		hostPortID = hostPort.ID
		f.database.hostPorts[node][port] = hostPortID

	}
	// Check if port is already attached
	for _, v := range f.database.attachedPorts[hostPortLabelID] {
		if _, ok = v[hostPortID]; ok {
			klog.Infof("hostPort %s already attached by association %s", hostPortID, v[hostPortID])
			return nil
		}
	}
	klog.Infof("Add hostPortLabel %s to host %s port %s", hostPortLabelID, node, port)
	hostPortAssociation := HostPortAssociation{
		DeploymentID:    f.deployment.ID,
		HostPortLabelID: hostPortLabelID,
		HostPortID:      hostPortID,
	}
	jsonRequest, _ := json.Marshal(hostPortAssociation)
	statusCode, jsonResponse, err := f.POST(hostPortAssociationPath, jsonRequest)
	if err != nil {
		return err
	}
	if statusCode != 201 {
		var errorResponse ErrorResponse
		json.Unmarshal(jsonResponse, &errorResponse)
		klog.Errorf("HostPortAssociation error: %+v", errorResponse)
		return fmt.Errorf("Create HostPortAssociation failed with code %d", statusCode)
	}
	json.Unmarshal(jsonResponse, &hostPortAssociation)
	klog.Infof("HostPortAssociation is created: %+v", hostPortAssociation)
	portAssociation := make(HostPortAssociationIDByPort)
	portAssociation[hostPortID] = hostPortAssociation.ID
	f.database.attachedPorts[hostPortLabelID] = append(f.database.attachedPorts[hostPortLabelID], portAssociation)
	return nil
}

func (f *FssClient) DetachHostPort(hostPortLabelID string, node string, port string, nodeDeleted bool) error {
	// Check if port exists
	hostPortID, ok := f.database.hostPorts[node][port]
	if ok {
		klog.Infof("Remove hostPortLabel %s from host %s port %s", hostPortLabelID, node, port)
		for k, v := range f.database.attachedPorts[hostPortLabelID] {
			if hostPortAssociationID, ok := v[hostPortID]; ok {
				u := hostPortAssociationPath + "/" + hostPortAssociationID
				statusCode, _, err := f.DELETE(u)
				if err != nil {
					return err
				}
				if statusCode != 204 {
					return fmt.Errorf("Delete HostPortAssociation failed with code %d", statusCode)
				}
				klog.Infof("HostPortAssociation %s is deleted", hostPortAssociationID)
				// Remove locally
				f.database.attachedPorts[hostPortLabelID] = append(f.database.attachedPorts[hostPortLabelID][:k], f.database.attachedPorts[hostPortLabelID][k+1:]...)
			}
		}
	}
	if nodeDeleted {
		delete(f.database.hostPorts, node)
	}
	return nil
}
