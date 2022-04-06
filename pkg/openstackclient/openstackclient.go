package openstackclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/utils/openstack/clientconfig"

	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/util/cert"
	"k8s.io/klog"
)

type AuthOpts struct {
	AuthURL      string                   `gcfg:"auth-url" mapstructure:"auth-url" name:"os-authURL" dependsOn:"os-password|os-trustID|os-applicationCredentialSecret|os-clientCertPath"`
	UserID       string                   `gcfg:"user-id" mapstructure:"user-id" name:"os-userID" value:"optional" dependsOn:"os-password"`
	Username     string                   `name:"os-userName" value:"optional" dependsOn:"os-password"`
	Password     string                   `name:"os-password" value:"optional" dependsOn:"os-domainID|os-domainName,os-projectID|os-projectName,os-userID|os-userName"`
	TenantID     string                   `gcfg:"tenant-id" mapstructure:"project-id" name:"os-projectID" value:"optional" dependsOn:"os-password|os-clientCertPath"`
	TenantName   string                   `gcfg:"tenant-name" mapstructure:"project-name" name:"os-projectName" value:"optional" dependsOn:"os-password|os-clientCertPath"`
	DomainID     string                   `gcfg:"domain-id" mapstructure:"domain-id" name:"os-domainID" value:"optional" dependsOn:"os-password|os-clientCertPath"`
	DomainName   string                   `gcfg:"domain-name" mapstructure:"domain-name" name:"os-domainName" value:"optional" dependsOn:"os-password|os-clientCertPath"`
	Region       string                   `name:"os-region"`
	EndpointType gophercloud.Availability `gcfg:"os-endpoint-type" mapstructure:"os-endpoint-type" name:"os-endpointType" value:"optional"`
	CAFile       string                   `gcfg:"ca-file" mapstructure:"ca-file" name:"os-certAuthorityPath" value:"optional"`
	TLSInsecure  string                   `gcfg:"tls-insecure" mapstructure:"tls-insecure" name:"os-TLSInsecure" value:"optional" matches:"^true|false$"`
}

func (authOpts AuthOpts) ToAuthOptions() gophercloud.AuthOptions {
	opts := clientconfig.ClientOpts{
		// this is needed to disable the clientconfig.AuthOptions func env detection
		EnvPrefix: "_",
		AuthInfo: &clientconfig.AuthInfo{
			AuthURL:     authOpts.AuthURL,
			UserID:      authOpts.UserID,
			Username:    authOpts.Username,
			Password:    authOpts.Password,
			ProjectID:   authOpts.TenantID,
			ProjectName: authOpts.TenantName,
			DomainID:    authOpts.DomainID,
			DomainName:  authOpts.DomainName,
		},
	}

	ao, err := clientconfig.AuthOptions(&opts)
	if err != nil {
		klog.Errorf("Error parsing auth: %s", err)
		return gophercloud.AuthOptions{}
	}

	// Persistent service, so we need to be able to renew tokens.
	ao.AllowReauth = true

	return *ao
}

// NewOpenStackClient creates a new instance of the openstack client
func NewOpenStackClient(cfg *AuthOpts, userAgent string, extraUserAgent ...string) (*gophercloud.ProviderClient, error) {
	provider, err := openstack.NewClient(cfg.AuthURL)
	if err != nil {
		return nil, err
	}

	var caPool *x509.CertPool
	if cfg.CAFile != "" {
		// read and parse CA certificate from file
		caPool, err = cert.NewPool(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read and parse %s certificate: %s", cfg.CAFile, err)
		}
	}

	config := &tls.Config{}
	config.InsecureSkipVerify = cfg.TLSInsecure == "true"

	if caPool != nil {
		config.RootCAs = caPool
	}

	provider.HTTPClient.Transport = net.SetOldTransportDefaults(&http.Transport{TLSClientConfig: config})

	opts := cfg.ToAuthOptions()
	err = openstack.Authenticate(provider, opts)

	return provider, err
}

// NewNetworkV2 creates a ServiceClient that may be used with the neutron v2 API
func NewNetworkV2(provider *gophercloud.ProviderClient, eo *gophercloud.EndpointOpts) (*gophercloud.ServiceClient, error) {
	network, err := openstack.NewNetworkV2(provider, *eo)
	if err != nil {
		return nil, fmt.Errorf("failed to find network v2 %s endpoint for region %s: %v", eo.Availability, eo.Region, err)
	}
	return network, nil
}

// NewComputeV2 creates a ServiceClient that may be used with the nova v2 API
func NewComputeV2(provider *gophercloud.ProviderClient, eo *gophercloud.EndpointOpts) (*gophercloud.ServiceClient, error) {
	compute, err := openstack.NewComputeV2(provider, *eo)
	if err != nil {
		return nil, fmt.Errorf("failed to find compute v2 %s endpoint for region %s: %v", eo.Availability, eo.Region, err)
	}
	return compute, nil
}
