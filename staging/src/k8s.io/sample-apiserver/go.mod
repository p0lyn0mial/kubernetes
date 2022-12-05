// This is a generated file. Do not edit directly.

module k8s.io/sample-apiserver

go 1.16

require (
	github.com/google/gofuzz v1.1.0
	github.com/kcp-dev/logicalcluster/v2 v2.0.0-alpha.1
	github.com/spf13/cobra v1.4.0
	k8s.io/apimachinery v0.24.3
	k8s.io/apiserver v0.0.0
	k8s.io/client-go v0.24.3
	k8s.io/code-generator v0.0.0
	k8s.io/component-base v0.0.0
	k8s.io/kube-openapi v0.0.0-20220328201542-3ee0da9b0b42
	k8s.io/utils v0.0.0-20220728103510-ee6ede2d64ed
)

replace (
	github.com/go-logr/logr => github.com/go-logr/logr v1.2.0
	github.com/google/go-cmp => github.com/google/go-cmp v0.5.5

	github.com/kcp-dev/apimachinery => github.com/p0lyn0mial/apimachinery v0.0.0-20221205120156-b90e084cbd56
	github.com/kcp-dev/client-go => github.com/p0lyn0mial/client-go v0.0.0-20221205150201-dfa4fc4b06bb
	github.com/kcp-dev/logicalcluster/v3 => github.com/p0lyn0mial/logicalcluster/v3 v3.0.0-20221205140724-b21d40759226
	github.com/stretchr/testify => github.com/stretchr/testify v1.7.0
	golang.org/x/net => golang.org/x/net v0.0.0-20220127200216-cd36cc0744dd
	golang.org/x/sys => golang.org/x/sys v0.0.0-20220209214540-3681064d5158
	google.golang.org/protobuf => google.golang.org/protobuf v1.27.1
	k8s.io/api => ../api
	k8s.io/apimachinery => ../apimachinery
	k8s.io/apiserver => ../apiserver
	k8s.io/client-go => ../client-go
	k8s.io/code-generator => ../code-generator
	k8s.io/component-base => ../component-base
	k8s.io/klog/v2 => k8s.io/klog/v2 v2.60.1
	k8s.io/sample-apiserver => ../sample-apiserver
	k8s.io/utils => k8s.io/utils v0.0.0-20220210201930-3a6ce19ff2f9
	sigs.k8s.io/json => sigs.k8s.io/json v0.0.0-20211208200746-9f7c6b3444d2
	sigs.k8s.io/structured-merge-diff/v4 => sigs.k8s.io/structured-merge-diff/v4 v4.2.1
	sigs.k8s.io/yaml => sigs.k8s.io/yaml v1.2.0
)
