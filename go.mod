module github.com/neondatabase/autoscaling

go 1.24.0

replace (
	github.com/optiopay/kafka => github.com/optiopay/kafka v0.0.0
	k8s.io/api => k8s.io/api v0.31.7
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.31.7
	k8s.io/apimachinery => k8s.io/apimachinery v0.31.7
	k8s.io/apiserver => k8s.io/apiserver v0.31.7
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.31.7
	k8s.io/client-go => k8s.io/client-go v0.31.7
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.31.7
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.31.7
	k8s.io/code-generator => k8s.io/code-generator v0.31.7
	k8s.io/component-base => k8s.io/component-base v0.31.7
	k8s.io/component-helpers => k8s.io/component-helpers v0.31.7
	k8s.io/controller-manager => k8s.io/controller-manager v0.31.7
	k8s.io/cri-api => k8s.io/cri-api v0.31.7
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.31.7
	k8s.io/dynamic-resource-allocation => k8s.io/dynamic-resource-allocation v0.31.7
	k8s.io/endpointslice => k8s.io/endpointslice v0.31.7
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.31.7
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.31.7
	k8s.io/kube-openapi => k8s.io/kube-openapi v0.0.0-20240228011516-70dd3763d340
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.31.7
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.31.7
	k8s.io/kubectl => k8s.io/kubectl v0.31.7
	k8s.io/kubelet => k8s.io/kubelet v0.31.7
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.31.7
	k8s.io/metrics => k8s.io/metrics v0.31.7
	k8s.io/mount-utils => k8s.io/mount-utils v0.31.7
	k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.31.7
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.31.7
	k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.31.7
	k8s.io/sample-controller => k8s.io/sample-controller v0.31.7
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.14.0
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.7.0
	github.com/Azure/azure-sdk-for-go/sdk/storage/azblob v1.3.2
	github.com/alessio/shellescape v1.4.1
	github.com/aws/aws-sdk-go-v2/config v1.27.36
	github.com/aws/aws-sdk-go-v2/service/s3 v1.53.2
	github.com/cert-manager/cert-manager v1.16.4
	github.com/cilium/cilium v1.12.14
	github.com/containerd/cgroups/v3 v3.0.1
	github.com/containernetworking/cni v1.1.1
	github.com/coreos/go-iptables v0.6.0
	github.com/digitalocean/go-qemu v0.0.0-20220826173844-d5f5e3ceed89
	github.com/docker/cli v25.0.3+incompatible
	github.com/docker/docker v24.0.9+incompatible
	github.com/docker/libnetwork v0.8.0-dev.2.0.20210525090646-64b7a4574d14
	github.com/go-logr/logr v1.4.2
	github.com/go-logr/zapr v1.3.0
	github.com/jpillora/backoff v1.0.0
	github.com/k8snetworkplumbingwg/network-attachment-definition-client v1.4.0
	github.com/k8snetworkplumbingwg/whereabouts v0.6.1
	github.com/kdomanski/iso9660 v0.3.3
	github.com/lithammer/shortuuid v3.0.0+incompatible
	github.com/onsi/ginkgo/v2 v2.19.0
	github.com/onsi/gomega v1.33.1
	github.com/opencontainers/runtime-spec v1.0.3-0.20220909204839-494a5a6aca78
	github.com/orlangure/gnomock v0.30.0
	github.com/prometheus/client_golang v1.20.4
	github.com/prometheus/client_model v0.6.1
	github.com/prometheus/common v0.55.0
	github.com/samber/lo v1.39.0
	github.com/stretchr/testify v1.9.0
	github.com/tychoish/fun v0.8.5
	github.com/vishvananda/netlink v1.1.1-0.20220125195016-0639e7e787ba
	go.uber.org/multierr v1.11.0
	go.uber.org/zap v1.27.0
	golang.org/x/crypto v0.36.0
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56
	golang.org/x/sync v0.12.0
	golang.org/x/term v0.30.0
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/api v0.31.7
	k8s.io/apimachinery v0.31.7
	k8s.io/apiserver v0.31.7
	k8s.io/client-go v0.31.7
	k8s.io/klog/v2 v2.130.1
	k8s.io/kubernetes v1.31.7
	sigs.k8s.io/controller-runtime v0.19.7 // should match k8s dependencies versions
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.10.0 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20210617225240-d185dfc1b5a1 // indirect
	github.com/Azure/go-ntlmssp v0.0.0-20221128193559-754e69321358 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.2.2 // indirect
	github.com/Microsoft/go-winio v0.6.0 // indirect
	github.com/NYTimes/gziphandler v1.1.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/asaskevich/govalidator v0.0.0-20230301143203-a9d515a09cc2 // indirect
	github.com/aws/aws-sdk-go-v2 v1.31.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.2 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.17.34 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.14 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.18 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.18 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.3.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.11.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.3.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.11.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.17.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.23.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.27.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.31.0 // indirect
	github.com/aws/smithy-go v1.21.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cilium/ebpf v0.10.0 // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/digitalocean/go-libvirt v0.0.0-20220804181439-8648fbde413e // indirect
	github.com/distribution/reference v0.5.0
	github.com/docker/distribution v2.8.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.8.1 // indirect
	github.com/docker/go-connections v0.4.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/emicklei/go-restful/v3 v3.12.1 // indirect
	github.com/evanphx/json-patch/v5 v5.9.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/fsnotify/fsnotify v1.7.0 // indirect
	github.com/fxamacker/cbor/v2 v2.7.0 // indirect
	github.com/go-asn1-ber/asn1-ber v1.5.6 // indirect
	github.com/go-ldap/ldap/v3 v3.4.8 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/jsonreference v0.21.0 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.2.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/cel-go v0.20.1 // indirect
	github.com/google/gnostic-models v0.6.9-0.20230804172637-c7be7c783f49 // indirect
	github.com/google/go-cmp v0.6.0 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/google/pprof v0.0.0-20240525223248-4bfdf5a9a2af // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.20.0 // indirect
	github.com/imdario/mergo v0.3.16 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/ishidawataru/sctp v0.0.0-20230406120618-7ff4192f6ff2 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/moby/sys/mountinfo v0.7.1 // indirect
	github.com/moby/term v0.5.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/morikuni/aec v1.0.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.0-rc2 // indirect
	github.com/opencontainers/runc v1.1.13 // indirect
	github.com/opencontainers/selinux v1.11.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/spf13/cobra v1.8.1 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/stoewer/go-strcase v1.3.0 // indirect
	github.com/vishvananda/netns v0.0.4 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.etcd.io/etcd/api/v3 v3.5.14 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.14 // indirect
	go.etcd.io/etcd/client/v3 v3.5.14 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.54.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.54.0 // indirect
	go.opentelemetry.io/otel v1.29.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.28.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.27.0 // indirect
	go.opentelemetry.io/otel/metric v1.29.0 // indirect
	go.opentelemetry.io/otel/sdk v1.28.0 // indirect
	go.opentelemetry.io/otel/trace v1.29.0 // indirect
	go.opentelemetry.io/proto/otlp v1.3.1 // indirect
	golang.org/x/mod v0.20.0 // indirect
	golang.org/x/net v0.38.0 // indirect
	golang.org/x/oauth2 v0.23.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/text v0.23.0 // indirect
	golang.org/x/time v0.6.0 // indirect
	golang.org/x/tools v0.24.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.4.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240827150818-7e3bb234dfed // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240903143218-8af14fe29dc1 // indirect
	google.golang.org/grpc v1.66.2 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.12.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gotest.tools/v3 v3.5.1 // indirect
	k8s.io/apiextensions-apiserver v0.31.1 // indirect
	k8s.io/cloud-provider v0.0.0 // indirect
	k8s.io/component-base v0.31.7 // indirect
	k8s.io/component-helpers v0.31.7 // indirect
	k8s.io/controller-manager v0.31.7 // indirect
	k8s.io/csi-translation-lib v0.0.0 // indirect
	k8s.io/dynamic-resource-allocation v0.0.0 // indirect
	k8s.io/kms v0.31.7 // indirect
	k8s.io/kube-openapi v0.0.0-20240903163716-9e1beecbcb38 // indirect
	k8s.io/kube-scheduler v0.0.0 // indirect
	k8s.io/kubelet v0.31.7 // indirect
	k8s.io/mount-utils v0.0.0 // indirect
	k8s.io/utils v0.0.0-20240921022957-49e7df575cb6 // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.30.3 // indirect
	sigs.k8s.io/gateway-api v1.1.0 // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.4.1 // indirect
	sigs.k8s.io/yaml v1.4.0 // indirect
)

require github.com/coder/websocket v1.8.13
