package emprovider

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/metrics/pkg/apis/external_metrics"

	// TODO: Vendor this
	cmaprovider "github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/custom-metrics-apiserver/pkg/provider"

	"github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/sdc"
)

type sysdigProvider struct {
	mapper               apimeta.RESTMapper
	kubeClient           dynamic.ClientPool
	sysdigClient         *sdc.Client
	sysdigRequestTimeout time.Duration

	MetricsRegistry
}

func NewSysdigProvider(mapper apimeta.RESTMapper, kubeClient dynamic.ClientPool, sysdigClient *sdc.Client, sysdigRequestTimeout time.Duration, updateInterval time.Duration, stopChan <-chan struct{}) cmaprovider.ExternalMetricsProvider {
	lister := &cachingMetricsLister{
		sysdigClient:         sysdigClient,
		sysdigRequestTimeout: sysdigRequestTimeout,
		updateInterval:       updateInterval,
		MetricsRegistry:      &registry{},
	}
	lister.RunUntil(stopChan)
	return &sysdigProvider{
		kubeClient:           kubeClient,
		mapper:               mapper,
		sysdigClient:         sysdigClient,
		sysdigRequestTimeout: sysdigRequestTimeout,
		MetricsRegistry:      lister,
	}
}

func (p *sysdigProvider) LabelsSelectorToMap(metricSelector labels.Selector) map[string]string {
	strPairs := strings.Split(metricSelector.String(), ",")
	mapLabels := make(map[string]string, len(strPairs))
	for _, pair := range strPairs {
		kv := strings.Split(pair, "=")
		if len(kv) != 2 {
			continue
		}

		mapLabels[kv[0]] = kv[1]
	}
	return mapLabels
}

func (p *sysdigProvider) GetExternalMetric(namespace string, metricName string, metricSelector labels.Selector) (*external_metrics.ExternalMetricValueList, error) {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	matchingMetrics := []external_metrics.ExternalMetricValue{}
	mapLabels := p.LabelsSelectorToMap(metricSelector)

	ctx, cancel := context.WithTimeout(context.Background(), p.sysdigRequestTimeout)
	defer cancel()
	req := &sdc.GetDataRequest{Last: 10, Sampling: 10}
	filterArr := []string{}
	for k, v := range mapLabels {
		filterArr = append(filterArr, fmt.Sprintf("%s='%s'", k, v))
	}

	req = req.
		//WithMetric(metricName, &sdc.MetricAggregation{Group: "avg", Time: "timeAvg"}).
		WithMetric(metricName, nil).
		WithFilter(strings.Join(filterArr, " and "))
	log.Printf("GetExternalMetrics with req:%+v", req)
	payload, _, err := p.sysdigClient.Data.Get(ctx, req)
	if err != nil {
		log.Printf("Error occurred when get data, err: %v", err)
		return &external_metrics.ExternalMetricValueList{
			Items: matchingMetrics,
		}, fmt.Errorf("sysdig client error: %v", err)
	}
	if len(payload.Samples) == 0 {
		return &external_metrics.ExternalMetricValueList{
			Items: matchingMetrics,
		}, nil
	}
	val, err := payload.FirstValue()
	if err != nil {
		return &external_metrics.ExternalMetricValueList{
			Items: matchingMetrics,
		}, cmaprovider.NewExternalMetricNotFoundError(metricName)
	}
	float, err := strconv.ParseFloat(string(val), 64)
	if err != nil {
		return &external_metrics.ExternalMetricValueList{
			Items: matchingMetrics,
		}, fmt.Errorf("sysdig client returned a value that cannot be parsed as a float: %v", string(val))
	}
	metricValue := external_metrics.ExternalMetricValue{
		MetricName:   metricName,
		MetricLabels: mapLabels,
		Value:        *resource.NewMilliQuantity(int64(float*1000), resource.DecimalSI),
		Timestamp:    metav1.Time{Time: time.Now()},
	}
	matchingMetrics = append(matchingMetrics, metricValue)

	return &external_metrics.ExternalMetricValueList{
		Items: matchingMetrics,
	}, nil
}

type cachingMetricsLister struct {
	sysdigClient         *sdc.Client
	sysdigRequestTimeout time.Duration
	updateInterval       time.Duration

	MetricsRegistry
}

func (l *cachingMetricsLister) Run() {
	l.RunUntil(wait.NeverStop)
}

func (l *cachingMetricsLister) RunUntil(stopChan <-chan struct{}) {
	go wait.Until(func() {
		if err := l.updateMetrics(); err != nil {
			utilruntime.HandleError(err)
		}
	}, l.updateInterval, stopChan)
}

func (l *cachingMetricsLister) updateMetrics() error {
	ctx, cancel := context.WithTimeout(context.Background(), l.sysdigRequestTimeout)
	defer cancel()
	metrics, _, err := l.sysdigClient.Data.Metrics(ctx)
	if err != nil {
		return fmt.Errorf("unable to fetch list of all available metrics: %v", err)
	}
	l.UpdateMetrics(metrics)
	return nil
}
