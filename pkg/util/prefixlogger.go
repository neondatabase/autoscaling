package util

import (
	"fmt"

	"k8s.io/klog/v2"
)

type PrefixLogger struct {
	Prefix string
}

func (l PrefixLogger) Infof(format string, args ...interface{}) {
	klog.InfofDepth(1, fmt.Sprint(l.Prefix, format), args...)
}

func (l PrefixLogger) Warningf(format string, args ...interface{}) {
	klog.WarningfDepth(1, fmt.Sprint(l.Prefix, format), args...)
}

func (l PrefixLogger) Errorf(format string, args ...interface{}) {
	klog.ErrorfDepth(1, fmt.Sprint(l.Prefix, format), args...)
}

func (l PrefixLogger) Fatalf(format string, args ...interface{}) {
	klog.FatalfDepth(1, fmt.Sprint(l.Prefix, format), args...)
}
