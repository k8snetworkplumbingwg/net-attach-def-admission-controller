module github.com/k8snetworkplumbingwg/net-attach-def-admission-controller

go 1.12

require (
	github.com/containernetworking/cni v0.8.1
	github.com/evanphx/json-patch v4.5.0+incompatible // indirect
	github.com/golang/glog v0.0.0-20160126235308-23def4e6c14b
	github.com/googleapis/gnostic v0.3.1 // indirect
	github.com/intel/multus-cni v0.0.0-20200323144905-7f50f5f17526
	github.com/k8snetworkplumbingwg/network-attachment-definition-client v0.0.0-20200127152046-0ee521d56061
	github.com/onsi/ginkgo v1.10.1
	github.com/onsi/gomega v1.7.0
	github.com/pkg/errors v0.8.1
	github.com/prometheus/client_golang v1.2.1
	golang.org/x/text v0.3.3 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	k8s.io/api v0.0.0-20190918195907-bd6ac527cfd2
	k8s.io/apimachinery v0.0.0-20190817020851-f2f3a405f61d
	k8s.io/client-go v0.0.0-20190918200256-06eb1244587a
	k8s.io/utils v0.0.0-20190506122338-8fab8cb257d5 // indirect
)
