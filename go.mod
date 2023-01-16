module github.com/neondatabase/autoscaling

go 1.19

replace (
	k8s.io/api => k8s.io/api v0.23.15
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.23.15
	k8s.io/apimachinery => k8s.io/apimachinery v0.23.15
	k8s.io/apiserver => k8s.io/apiserver v0.23.15
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.23.15
	k8s.io/client-go => k8s.io/client-go v0.23.15
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.23.15
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.23.15
	k8s.io/code-generator => k8s.io/code-generator v0.23.15
	k8s.io/component-base => k8s.io/component-base v0.23.15
	k8s.io/component-helpers => k8s.io/component-helpers v0.23.15
	k8s.io/controller-manager => k8s.io/controller-manager v0.23.15
	k8s.io/cri-api => k8s.io/cri-api v0.23.15
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.23.15
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.23.15
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.23.15
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.23.15
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.23.15
	k8s.io/kubectl => k8s.io/kubectl v0.23.15
	k8s.io/kubelet => k8s.io/kubelet v0.23.15
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.23.15
	k8s.io/metrics => k8s.io/metrics v0.23.15
	k8s.io/mount-utils => k8s.io/mount-utils v0.23.15
	k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.23.15
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.23.15
	k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.23.15
	k8s.io/sample-controller => k8s.io/sample-controller v0.23.15
)

require (
	github.com/google/uuid v1.3.0
	github.com/neondatabase/neonvm v0.0.0-20221218221638-078f50be5f77
	golang.org/x/exp v0.0.0-20221126150942-6ab00d035af9
	k8s.io/api v0.23.15
	k8s.io/apimachinery v0.23.15
	k8s.io/client-go v0.23.15
	k8s.io/klog/v2 v2.70.1
	k8s.io/kubernetes v1.23.15
)

require (
	github.com/Azure/go-ansiterm v0.0.0-20210617225240-d185dfc1b5a1 // indirect
	github.com/NYTimes/gziphandler v1.1.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bits-and-blooms/bitset v1.2.0 // indirect
	github.com/blang/semver v3.5.1+incompatible // indirect
	github.com/cespare/xxhash/v2 v2.1.2 // indirect
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.3.2 // indirect
	github.com/cyphar/filepath-securejoin v0.2.2 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/docker/distribution v2.7.1+incompatible // indirect
	github.com/emicklei/go-restful v2.9.5+incompatible // indirect
	github.com/evanphx/json-patch v5.6.0+incompatible // indirect
	github.com/felixge/httpsnoop v1.0.1 // indirect
	github.com/fsnotify/fsnotify v1.5.4 // indirect
	github.com/go-logr/logr v1.2.3 // indirect
	github.com/go-openapi/jsonpointer v0.19.5 // indirect
	github.com/go-openapi/jsonreference v0.20.0 // indirect
	github.com/go-openapi/swag v0.21.1 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/golang/protobuf v1.5.2 // indirect
	github.com/google/go-cmp v0.5.8 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/googleapis/gnostic v0.5.5 // indirect
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway v1.16.0 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/inconshreveable/mousetrap v1.0.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.2-0.20181231171920-c182affec369 // indirect
	github.com/moby/term v0.0.0-20210610120745-9d4ed1856297 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/runc v1.0.2 // indirect
	github.com/opencontainers/selinux v1.8.2 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/prometheus/client_golang v1.12.2 // indirect
	github.com/prometheus/client_model v0.2.1-0.20210607210712-147c58e9608a // indirect
	github.com/prometheus/common v0.32.1 // indirect
	github.com/prometheus/procfs v0.7.3 // indirect
	github.com/spf13/cobra v1.4.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	go.etcd.io/etcd/api/v3 v3.5.0 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.0 // indirect
	go.etcd.io/etcd/client/v3 v3.5.0 // indirect
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
	go.uber.org/atomic v1.9.0 // indirect
	go.uber.org/multierr v1.8.0 // indirect
	go.uber.org/zap v1.21.0 // indirect
	golang.org/x/crypto v0.0.0-20220411220226-7b82a4e95df4 // indirect
	golang.org/x/net v0.0.0-20220722155237-a158d28d115b // indirect
	golang.org/x/oauth2 v0.0.0-20220411215720-9780585627b5 // indirect
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c // indirect
	golang.org/x/sys v0.1.0 // indirect
	golang.org/x/term v0.0.0-20210927222741-03fcf44c2211 // indirect
	golang.org/x/text v0.3.7 // indirect
	golang.org/x/time v0.0.0-20220609170525-579cf78fd858 // indirect
	gomodules.xyz/jsonpatch/v2 v2.2.0 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/genproto v0.0.0-20210924002016-3dee208752a0 // indirect
	google.golang.org/grpc v1.40.0 // indirect
	google.golang.org/protobuf v1.28.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.0.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/apiextensions-apiserver v0.23.10 // indirect
	k8s.io/apiserver v0.23.15 // indirect
	k8s.io/cloud-provider v0.23.15 // indirect
	k8s.io/component-base v0.23.15 // indirect
	k8s.io/component-helpers v0.23.15 // indirect
	k8s.io/csi-translation-lib v0.23.15 // indirect
	k8s.io/kube-openapi v0.0.0-20211115234752-e816edb12b65 // indirect
	k8s.io/kube-scheduler v0.0.0 // indirect
	k8s.io/mount-utils v0.23.15 // indirect
	k8s.io/utils v0.0.0-20211116205334-6203023598ed // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.33 // indirect
	sigs.k8s.io/controller-runtime v0.11.2 // indirect
	sigs.k8s.io/json v0.0.0-20211020170558-c049b76a60c6 // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.2.3 // indirect
	sigs.k8s.io/yaml v1.3.0 // indirect
)
