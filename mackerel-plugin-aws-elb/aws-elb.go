package main

import (
	"errors"
	"flag"
	"github.com/crowdmob/goamz/aws"
	"github.com/crowdmob/goamz/cloudwatch"
	mp "github.com/mackerelio/go-mackerel-plugin"
	"log"
	"os"
	"time"
)

var graphdef map[string](mp.Graphs) = map[string](mp.Graphs){
	"elb.latency": mp.Graphs{
		Label: "Whole ELB Latency",
		Unit:  "float",
		Metrics: [](mp.Metrics){
			mp.Metrics{Name: "Latency", Label: "Latency"},
		},
	},
	"elb.http_backend": mp.Graphs{
		Label: "Whole ELB HTTP Backend Count",
		Unit:  "integer",
		Metrics: [](mp.Metrics){
			mp.Metrics{Name: "HTTPCode_Backend_2XX", Label: "2XX", Stacked: true},
			mp.Metrics{Name: "HTTPCode_Backend_3XX", Label: "3XX", Stacked: true},
			mp.Metrics{Name: "HTTPCode_Backend_4XX", Label: "4XX", Stacked: true},
			mp.Metrics{Name: "HTTPCode_Backend_5XX", Label: "5XX", Stacked: true},
		},
	},

	// "elb.healthy_host_count", "elb.unhealthy_host_count" will be generated dynamically
}

type StatType int

const (
	Average StatType = iota
	Sum
)

func (s StatType) String() string {
	switch s {
	case Average:
		return "Average"
	case Sum:
		return "Sum"
	}
	return ""
}

type ELBPlugin struct {
	Region          string
	AccessKeyId     string
	SecretAccessKey string
	AZs             []string
	CloudWatch      *cloudwatch.CloudWatch
}

func (p *ELBPlugin) Prepare() error {
	auth, err := aws.GetAuth(p.AccessKeyId, p.SecretAccessKey, "", time.Now())
	if err != nil {
		return err
	}

	p.CloudWatch, err = cloudwatch.NewCloudWatch(auth, aws.Regions[p.Region].CloudWatchServicepoint)
	if err != nil {
		return err
	}

	ret, err := p.CloudWatch.ListMetrics(&cloudwatch.ListMetricsRequest{
		Namespace: "AWS/ELB",
		Dimensions: []cloudwatch.Dimension{
			cloudwatch.Dimension{
				Name: "AvailabilityZone",
			},
		},
		MetricName: "HealthyHostCount",
	})

	if err != nil {
		return err
	}

	p.AZs = make([]string, 0, len(ret.ListMetricsResult.Metrics))
	for _, met := range ret.ListMetricsResult.Metrics {
		if len(met.Dimensions) > 1 {
			continue
		} else if met.Dimensions[0].Name != "AvailabilityZone" {
			continue
		}

		p.AZs = append(p.AZs, met.Dimensions[0].Value)
	}

	return nil
}

func (p ELBPlugin) GetLastPoint(dimension *cloudwatch.Dimension, metricName string, statType StatType) (float64, error) {
	now := time.Now()

	response, err := p.CloudWatch.GetMetricStatistics(&cloudwatch.GetMetricStatisticsRequest{
		Dimensions: []cloudwatch.Dimension{*dimension},
		StartTime:  now.Add(time.Duration(120) * time.Second * -1), // 2 min (to fetch at least 1 data-point)
		EndTime:    now,
		MetricName: metricName,
		Period:     60,
		Statistics: []string{statType.String()},
		Namespace:  "AWS/ELB",
	})
	if err != nil {
		return 0, err
	}

	datapoints := response.GetMetricStatisticsResult.Datapoints
	if len(datapoints) == 0 {
		return 0, errors.New("fetched no datapoints")
	}

	latest := time.Unix(0, 0)
	var latestVal float64
	for _, dp := range datapoints {
		if dp.Timestamp.Before(latest) {
			continue
		}

		latest = dp.Timestamp
		switch statType {
		case Average:
			latestVal = dp.Average
		case Sum:
			latestVal = dp.Sum
		}
	}

	return latestVal, nil
}

func (p ELBPlugin) FetchMetrics() (map[string]float64, error) {
	stat := make(map[string]float64)

	// HostCount per AZ
	for _, az := range p.AZs {
		d := &cloudwatch.Dimension{
			Name:  "AvailabilityZone",
			Value: az,
		}

		for _, met := range []string{"HealthyHostCount", "UnHealthyHostCount"} {
			v, err := p.GetLastPoint(d, met, Average)
			if err == nil {
				stat[met+"_"+az] = v
			}
		}
	}

	glb := &cloudwatch.Dimension{
		Name:  "Service",
		Value: "ELB",
	}

	v, err := p.GetLastPoint(glb, "Latency", Average)
	if err == nil {
		stat["Latency"] = v
	}

	for _, met := range [...]string{"HTTPCode_Backend_2XX", "HTTPCode_Backend_3XX", "HTTPCode_Backend_4XX", "HTTPCode_Backend_5XX"} {
		v, err := p.GetLastPoint(glb, met, Sum)
		if err == nil {
			stat[met] = v
		}
	}

	return stat, nil
}

func (p ELBPlugin) GraphDefinition() map[string](mp.Graphs) {
	for _, grp := range [...]string{"elb.healthy_host_count", "elb.unhealthy_host_count"} {
		var name_pre string
		var label string
		switch grp {
		case "elb.healthy_host_count":
			name_pre = "HealthyHostCount_"
			label = "ELB Healthy Host Count"
		case "elb.unhealthy_host_count":
			name_pre = "UnHealthyHostCount_"
			label = "ELB Unhealthy Host Count"
		}

		var metrics [](mp.Metrics)
		for _, az := range p.AZs {
			metrics = append(metrics, mp.Metrics{Name: name_pre + az, Label: az, Stacked: true})
		}
		graphdef[grp] = mp.Graphs{
			Label:   label,
			Unit:    "integer",
			Metrics: metrics,
		}
	}

	return graphdef
}

func main() {
	optRegion := flag.String("region", "", "AWS Region")
	optAccessKeyId := flag.String("access-key-id", "", "AWS Access Key ID")
	optSecretAccessKey := flag.String("secret-access-key", "", "AWS Secret Access Key")
	optTempfile := flag.String("tempfile", "", "Temp file name")
	flag.Parse()

	var elb ELBPlugin

	if *optRegion == "" {
		elb.Region = aws.InstanceRegion()
	} else {
		elb.Region = *optRegion
	}

	elb.AccessKeyId = *optAccessKeyId
	elb.SecretAccessKey = *optSecretAccessKey

	err := elb.Prepare()
	if err != nil {
		log.Fatalln(err)
	}

	helper := mp.NewMackerelPlugin(elb)
	if *optTempfile != "" {
		helper.Tempfile = *optTempfile
	} else {
		helper.Tempfile = "/tmp/mackerel-plugin-elb"
	}

	if os.Getenv("MACKEREL_AGENT_PLUGIN_META") != "" {
		helper.OutputDefinitions()
	} else {
		helper.OutputValues()
	}
}
