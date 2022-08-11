package fssclient

import (
	"encoding/json"
)

type LoginResponse struct {
	AccessToken      string `json:"access_token"`
	IDToken          string `json:"id_token"`
	SessionState     string `json:"session_state"`
	Scope            string `json:"scope"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	NotBeforePolicy  int    `json:"not-before-policy"`
}

type Plugins []Plugin
type Plugin struct {
	ConnectType            string `json:"connectType"`
	Name                   string `json:"name"`
	SupportsNewDeployments bool   `json:"supportsNewDeployments"`
	ID                     string `json:"id"`
	/*
		ExternalID             string `json:"externalId",omitempty`
		APIKey                 string `json:"apiKey",omitempty`
		CallbackURL            string `json:"callbackUrl",omitempty`
		PossibleSettings       []struct {
			Description string `json:"description"`
			Example     string `json:"example"`
			Name        string `json:"name"`
			Required    bool   `json:"required"`
			Unique      bool   `json:"unique"`
		} `json:"possibleSettings",omitempty`
	*/
}

type Deployments []Deployment
type Deployment struct {
	AdminUp  bool   `json:"adminUp"`
	Name     string `json:"name"`
	PluginID string `json:"pluginID"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	/*
		ExternalID  string `json:"externalId",omitempty`
		Description string `json:"description",omitempty`
		Settings    struct {
			Password string `json:"password"`
			Username string `json:"username"`
		} `json:"settings",omitempty`
	*/
}

type Tenants []Tenant
type Tenant struct {
	DeploymentID      string `json:"deploymentId"`
	FssWorkloadEvpnID string `json:"fssWorkloadEvpnId"`
	Name              string `json:"name"`
	FssManaged        bool   `json:"fssManaged"`
	ID                string `json:"id"`
	Status            string `json:"status"`
	/*
		ExternalID        string `json:"externalId",omitempty`
		DeployedVersion   int    `json:"deployedVersion",omitempty`
		Version           int    `json:"version",omitempty`
	*/
}

type Subnets []Subnet
type Subnet struct {
	DeploymentID string `json:"deploymentId"`
	TenantID     string `json:"tenantId"`
	FssSubnetID  string `json:"fssSubnetId"`
	Name         string `json:"name"`
	FssManaged   bool   `json:"fssManaged"`
	ID           string `json:"id"`
	Status       string `json:"status"`
	/*
		ExternalID      string `json:"externalId",omitempty`
		DeployedVersion int    `json:"deployedVersion",omitempty`
		Version         int    `json:"version",omitempty`
	*/
}

type HostPortLabels []HostPortLabel
type HostPortLabel struct {
	DeploymentID string `json:"deploymentId"`
	Name         string `json:"name"`
	ID           string `json:"id"`
	Status       string `json:"status"`
	/*
		ExternalID      string `json:"externalId",omitempty`
		DeployedVersion int    `json:"deployedVersion",omitempty`
		Version         int    `json:"version",omitempty`
	*/
}

type SubnetAssociations []SubnetAssociation
type SubnetAssociation struct {
	DeploymentID    string `json:"deploymentId"`
	HostPortLabelID string `json:"hostPortLabelID"`
	SubnetID        string `json:"subnetId"`
	VlanType        string `json:"vlanType"`
	VlanValue       string `json:"vlanValue"`
	ID              string `json:"id"`
	Status          string `json:"status"`
	/*
		ExternalID      string `json:"externalId",omitempty`
		DeployedVersion int    `json:"deployedVersion",omitempty`
		Version         int    `json:"version",omitempty`
	*/
}

type HostPorts []HostPort
type HostPort struct {
	DeploymentID     string   `json:"deploymentId"`
	HostName         string   `json:"hostName"`
	PortName         string   `json:"portName"`
	Name             string   `json:"name"`
	ID               string   `json:"id"`
	IsLag            bool     `json:"isLag"`
	ParentHostPortID string   `json:"parentHostPortId"`
	Status           string   `json:"status"`
	EdgeMapIds       []string `json:"edgeMapIds"`
	/*
		MacAddress       string   `json:"macAddress"`
		ExternalID       string   `json:"externalId",omitempty`
		DeployedVersion  int      `json:"deployedVersion",omitempty`
		Version          int      `json:"version",omitempty`
	*/
}

type HostPortAssociations []HostPortAssociation
type HostPortAssociation struct {
	DeploymentID    string `json:"deploymentId"`
	HostPortID      string `json:"hostPortId"`
	HostPortLabelID string `json:"hostPortLabelId"`
	ID              string `json:"id"`
	Status          string `json:"status"`
	/*
		ExternalID      string `json:"externalId",omitempty`
		DeployedVersion int    `json:"deployedVersion",omitempty`
		Version         int    `json:"version",omitempty`
	*/
}

type ErrorResponse struct {
	AdditionalInfo string   `json:"additional_info"`
	Detail         string   `json:"detail"`
	Errors         []string `json:"errors"`
	ObjectRef      string   `json:"object_ref"`
	Status         int      `json:"status"`
	Title          string   `json:"title"`
	Type           string   `json:"type"`
}

type HostPortLabelIDByVlan map[int]string
type HostPortIDByName map[string]string
type HostPortAssociationIDByPort map[string]string

type Database struct {
	// Tenants by fssWorkloadEvpnId
	tenants map[string]Tenant
	// Subnets by fssSubnetId
	subnets map[string]Subnet
	// HostPortLabelID by fssSubnetId and VlanID
	hostPortLabels map[string]HostPortLabelIDByVlan
	// HostPortLabelID by fssSubnetId and VlanID
	attachedLabels map[string]HostPortLabelIDByVlan
	// HostPortID by HostName and PortName
	hostPorts map[string]HostPortIDByName
	// HostPortAssociationIDs by HostPortLabelID and HostPortID
	attachedPorts map[string][]HostPortAssociationIDByPort
}

type EncodedDatabase struct {
	Tenants        map[string]map[string]interface{}
	Subnets        map[string]map[string]interface{}
	HostPortLabels map[string]HostPortLabelIDByVlan
	AttachedLabels map[string]HostPortLabelIDByVlan
	HostPorts      map[string]HostPortIDByName
	AttachedPorts  map[string][]HostPortAssociationIDByPort
}

func (d *Database) encode() []byte {
	var encoded EncodedDatabase
	encoded.Tenants = make(map[string]map[string]interface{})
	encoded.Subnets = make(map[string]map[string]interface{})
	// tenants
	for k, v := range d.tenants {
		encoded.Tenants[k] = make(map[string]interface{})
		tmp1, _ := json.Marshal(v)
		var tmp2 map[string]interface{}
		json.Unmarshal(tmp1, &tmp2)
		encoded.Tenants[k] = tmp2
	}
	// subnets
	for k, v := range d.subnets {
		encoded.Subnets[k] = make(map[string]interface{})
		tmp1, _ := json.Marshal(v)
		var tmp2 map[string]interface{}
		json.Unmarshal(tmp1, &tmp2)
		encoded.Subnets[k] = tmp2
	}
	encoded.HostPortLabels = d.hostPortLabels
	encoded.AttachedLabels = d.attachedLabels
	encoded.HostPorts = d.hostPorts
	encoded.AttachedPorts = d.attachedPorts
	jsonString, _ := json.Marshal(encoded)
	return jsonString
}

func (d *Database) decode(jsonString []byte) (Database, error) {
	var decoded Database
	decoded.tenants = make(map[string]Tenant)
	decoded.subnets = make(map[string]Subnet)
	var encoded EncodedDatabase
	err := json.Unmarshal(jsonString, &encoded)
	if err != nil {
		return decoded, err
	}
	for k, v := range encoded.Tenants {
		tmp, err := json.Marshal(v)
		if err != nil {
			return decoded, err
		}
		var tenant Tenant
		err = json.Unmarshal(tmp, &tenant)
		if err != nil {
			return decoded, err
		}
		decoded.tenants[k] = tenant
	}
	for k, v := range encoded.Subnets {
		tmp, err := json.Marshal(v)
		if err != nil {
			return decoded, err
		}
		var subnet Subnet
		err = json.Unmarshal(tmp, &subnet)
		if err != nil {
			return decoded, err
		}
		decoded.subnets[k] = subnet
	}
	decoded.hostPortLabels = encoded.HostPortLabels
	decoded.attachedLabels = encoded.AttachedLabels
	decoded.hostPorts = encoded.HostPorts
	decoded.attachedPorts = encoded.AttachedPorts
	return decoded, nil
}
