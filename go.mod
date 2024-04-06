module github.com/neondatabase/autoscaling

go 1.21

// Replace directives from github.com/cilium/cilium. Keep in sync when updating Cilium!
replace (
	github.com/miekg/dns => github.com/cilium/dns v1.1.51-0.20220729113855-5b94b11b46fc
	github.com/optiopay/kafka => github.com/cilium/kafka v0.0.0-20180809090225-01ce283b732b
	go.universe.tf/metallb => github.com/cilium/metallb v0.1.1-0.20220829170633-5d7dfb1129f7
	sigs.k8s.io/controller-tools => github.com/cilium/controller-tools v0.10.0
)

replace (
	k8s.io/api => k8s.io/api v0.25.16
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.25.16
	k8s.io/apimachinery => k8s.io/apimachinery v0.25.16
	k8s.io/apiserver => k8s.io/apiserver v0.25.16
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.25.16
	k8s.io/client-go => k8s.io/client-go v0.25.16
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.25.16
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.25.16
	k8s.io/code-generator => k8s.io/code-generator v0.25.16
	k8s.io/component-base => k8s.io/component-base v0.25.16
	k8s.io/component-helpers => k8s.io/component-helpers v0.25.16
	k8s.io/controller-manager => k8s.io/controller-manager v0.25.16
	k8s.io/cri-api => k8s.io/cri-api v0.25.16
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.25.16
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.25.16
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.25.16
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.25.16
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.25.16
	k8s.io/kubectl => k8s.io/kubectl v0.25.16
	k8s.io/kubelet => k8s.io/kubelet v0.25.16
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.25.16
	k8s.io/metrics => k8s.io/metrics v0.25.16
	k8s.io/mount-utils => k8s.io/mount-utils v0.25.16
	k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.25.16
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.25.16
	k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.25.16
	k8s.io/sample-controller => k8s.io/sample-controller v0.25.16
)

require (
	github.com/alessio/shellescape v1.4.1
	github.com/aws/aws-sdk-go-v2/config v1.15.12
	github.com/aws/aws-sdk-go-v2/service/s3 v1.53.1
	github.com/cilium/cilium v1.12.14
	github.com/containerd/cgroups/v3 v3.0.1
	github.com/containernetworking/cni v1.1.1
	github.com/coreos/go-iptables v0.6.0
	github.com/digitalocean/go-qemu v0.0.0-20220826173844-d5f5e3ceed89
	github.com/docker/docker v24.0.9+incompatible
	github.com/docker/libnetwork v0.8.0-dev.2.0.20210525090646-64b7a4574d14
	github.com/go-logr/logr v1.2.3
	github.com/jpillora/backoff v1.0.0
	github.com/k8snetworkplumbingwg/network-attachment-definition-client v1.4.0
	github.com/k8snetworkplumbingwg/whereabouts v0.6.1
	github.com/kdomanski/iso9660 v0.3.3
	github.com/lithammer/shortuuid v3.0.0+incompatible
	github.com/onsi/ginkgo/v2 v2.6.1
	github.com/onsi/gomega v1.24.2
	github.com/opencontainers/runtime-spec v1.0.3-0.20210326190908-1c3f411f0417
	github.com/prometheus/client_golang v1.14.0
	github.com/stretchr/testify v1.8.1
	github.com/tychoish/fun v0.8.5
	github.com/vishvananda/netlink v1.1.1-0.20220125195016-0639e7e787ba
	go.uber.org/zap v1.24.0
	golang.org/x/crypto v0.21.0
	golang.org/x/exp v0.0.0-20230425010034-47ecfdc1ba53
	golang.org/x/sync v0.1.0
	golang.org/x/term v0.18.0
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/api v0.25.16
	k8s.io/apimachinery v0.25.16
	k8s.io/apiserver v0.25.16
	k8s.io/client-go v0.25.16
	k8s.io/klog/v2 v2.80.1
	k8s.io/kubernetes v1.25.16
	nhooyr.io/websocket v1.8.7
	sigs.k8s.io/controller-runtime v0.13.1
	sigs.k8s.io/controller-tools v0.10.0
)

require (
	cloud.google.com/go/compute v1.19.1 // indirect
	cloud.google.com/go/compute/metadata v0.2.3 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20210617225240-d185dfc1b5a1 // indirect
	github.com/Azure/go-autorest v14.2.0+incompatible // indirect
	github.com/Azure/go-autorest/autorest v0.11.27 // indirect
	github.com/Azure/go-autorest/autorest/adal v0.9.20 // indirect
	github.com/Azure/go-autorest/autorest/date v0.3.0 // indirect
	github.com/Azure/go-autorest/logger v0.2.1 // indirect
	github.com/Azure/go-autorest/tracing v0.6.0 // indirect
	github.com/BurntSushi/toml v1.2.1 // indirect
	github.com/Microsoft/go-winio v0.6.0 // indirect
	github.com/NYTimes/gziphandler v1.1.1 // indirect
	github.com/aws/aws-sdk-go-v2 v1.26.1 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.2 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.12.7 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.12.7 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.3.14 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.3.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.11.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.3.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.11.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.17.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.11.10 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.16.8 // indirect
	github.com/aws/smithy-go v1.20.2 // indirect
	github.com/benbjohnson/clock v1.3.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/cilium/ebpf v0.10.0 // indirect
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/digitalocean/go-libvirt v0.0.0-20220804181439-8648fbde413e // indirect
	github.com/docker/distribution v2.8.2+incompatible // indirect
	github.com/docker/go-connections v0.4.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/emicklei/go-restful/v3 v3.8.0 // indirect
	github.com/evanphx/json-patch v5.6.0+incompatible // indirect
	github.com/evanphx/json-patch/v5 v5.6.0 // indirect
	github.com/fatih/color v1.13.0 // indirect
	github.com/felixge/httpsnoop v1.0.1 // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-logr/zapr v1.2.3 // indirect
	github.com/go-openapi/jsonpointer v0.19.5 // indirect
	github.com/go-openapi/jsonreference v0.20.0 // indirect
	github.com/go-openapi/swag v0.21.1 // indirect
	github.com/gobuffalo/flect v0.3.0 // indirect
	github.com/godbus/dbus/v5 v5.0.6 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang-jwt/jwt/v4 v4.2.0 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/google/gnostic v0.6.9 // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway v1.16.0 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/inconshreveable/mousetrap v1.0.1 // indirect
	github.com/ishidawataru/sctp v0.0.0-20210707070123-9a39160e9062 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.10.3 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mattn/go-colorable v0.1.12 // indirect
	github.com/mattn/go-isatty v0.0.14 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.2-0.20181231171920-c182affec369 // indirect
	github.com/moby/sys/mountinfo v0.6.2 // indirect
	github.com/moby/term v0.0.0-20221205130635-1aeaba878587 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/morikuni/aec v1.0.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.0-rc2 // indirect
	github.com/opencontainers/selinux v1.10.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/client_model v0.3.0 // indirect
	github.com/prometheus/common v0.37.0 // indirect
	github.com/prometheus/procfs v0.8.0 // indirect
	github.com/sirupsen/logrus v1.9.0 // indirect
	github.com/spf13/cobra v1.6.1 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/vishvananda/netns v0.0.0-20211101163701-50045581ed74 // indirect
	go.etcd.io/etcd/api/v3 v3.5.6 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.6 // indirect
	go.etcd.io/etcd/client/v3 v3.5.6 // indirect
	go.opentelemetry.io/contrib v0.20.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.20.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.20.0 // indirect
	go.opentelemetry.io/otel v0.20.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp v0.20.0 // indirect
	go.opentelemetry.io/otel/metric v0.20.0 // indirect
	go.opentelemetry.io/otel/sdk v0.20.0 // indirect
	go.opentelemetry.io/otel/sdk/export/metric v0.20.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v0.20.0 // indirect
	go.opentelemetry.io/otel/trace v0.20.0 // indirect
	go.opentelemetry.io/proto/otlp v0.7.0 // indirect
	go.uber.org/atomic v1.10.0 // indirect
	go.uber.org/multierr v1.9.0 // indirect
	golang.org/x/mod v0.8.0 // indirect
	golang.org/x/net v0.23.0 // indirect
	golang.org/x/oauth2 v0.7.0 // indirect
	golang.org/x/sys v0.18.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	golang.org/x/time v0.0.0-20220609170525-579cf78fd858 // indirect
	golang.org/x/tools v0.6.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.2.0 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/genproto v0.0.0-20230410155749-daa745c078e1 // indirect
	google.golang.org/grpc v1.56.3 // indirect
	google.golang.org/protobuf v1.30.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.0.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	k8s.io/apiextensions-apiserver v0.25.16 // indirect
	k8s.io/cloud-provider v0.0.0 // indirect
	k8s.io/component-base v0.25.16 // indirect
	k8s.io/component-helpers v0.25.16 // indirect
	k8s.io/csi-translation-lib v0.0.0 // indirect
	k8s.io/kube-openapi v0.0.0-20220803162953-67bda5d908f1 // indirect
	k8s.io/kube-scheduler v0.0.0 // indirect
	k8s.io/mount-utils v0.0.0 // indirect
	k8s.io/utils v0.0.0-20221107191617-1a15be271d1d // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.37 // indirect
	sigs.k8s.io/json v0.0.0-20220713155537-f223a00ba0e2 // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.2.3 // indirect
	sigs.k8s.io/yaml v1.3.0 // indirect
)
