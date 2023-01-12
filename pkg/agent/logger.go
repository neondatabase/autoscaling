import (
	"fmt"

	"k8s.io/klog/v2"
)

type RunnerLogger struct {
	prefix string
}

func (l RunnerLogger) Infof(format string, args ...interface{}) {
	klog.InfofDepth(1, fmt.Sprint(l.prefix, format), args...)
}

func (l RunnerLogger) Warningf(format string, args ...interface{}) {
	klog.WarningfDepth(1, fmt.Sprint(l.prefix, format), args...)
}

func (l RunnerLogger) Errorf(format string, args ...interface{}) {
	klog.ErrorfDepth(1, fmt.Sprint(l.prefix, format), args...)
}

func (l RunnerLogger) Fatalf(format string, args ...interface{}) {
	klog.FatalfDepth(1, fmt.Sprint(l.prefix, format), args...)
}
