package controller

import (
	"fmt"
	"strings"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
)

type Route53 struct {
	*route53.Route53
}

func newRoute53(awsconfig *aws.Config) *Route53 {
	session, err := session.NewSession(awsconfig)
	if err != nil {
		glog.Errorf("Failed to create AWS session. Error: %s.", err.Error())
		AWSErrorCount.With(prometheus.Labels{"service": "Route53"}).Add(float64(1))
		return nil
	}

	r53 := Route53{
		route53.New(session),
	}
	return &r53
}

// getDomain looks for the 'domain' of the hostname
// It assumes an ingress resource defined is only adding a single subdomain
// on to an AWS hosted zone. This may be too naive for Ticketmaster's use case
// TODO: review this approach.
func (r *Route53) getDomain(hostname string) (string, error) {
	hostname = strings.TrimSuffix(hostname, ".")
	domainParts := strings.Split(hostname, ".")
	if len(domainParts) < 2 {
		return "", fmt.Errorf("%s hostname does not contain a domain", hostname)
	}

	domain := strings.Join(domainParts[1:], ".")

	return strings.ToLower(domain), nil
}

// getZoneID looks for the Route53 zone ID of the hostname passed to it
// some voodoo is involved when stripping the domain
func (r *Route53) getZoneID(hostname string) (*route53.HostedZone, error) {
	zone, err := r.getDomain(hostname)
	if err != nil {
		return nil, err
	}

	glog.Infof("Fetching Zones matching %s", zone)
	resp, err := r.ListHostedZonesByName(
		&route53.ListHostedZonesByNameInput{
			DNSName: aws.String(zone),
		})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			AWSErrorCount.With(prometheus.Labels{"service": "Route53"}).Add(float64(1))
			return nil, fmt.Errorf("Error calling route53.ListHostedZonesByName: %s", awsErr.Code())
		}
		AWSErrorCount.With(prometheus.Labels{"service": "Route53"}).Add(float64(1))
		return nil, fmt.Errorf("Error calling route53.ListHostedZonesByName: %s", err)
	}

	if len(resp.HostedZones) == 0 {
		glog.Errorf("Unable to find the %s zone in Route53", zone)
		AWSErrorCount.With(prometheus.Labels{"service": "Route53"}).Add(float64(1))
		return nil, fmt.Errorf("Zone not found")
	}

	for _, i := range resp.HostedZones {
		zoneName := strings.TrimSuffix(*i.Name, ".")
		if zone == zoneName {
			glog.Infof("Found DNS Zone %s with ID %s", zoneName, *i.Id)
			return i, nil
		}
	}
	AWSErrorCount.With(prometheus.Labels{"service": "Route53"}).Add(float64(1))
	return nil, fmt.Errorf("Unable to find the zone: %s", zone)
}

func (r *Route53) upsertRecord(alb *albIngress) error {
	err := r.modifyRecord(alb, "UPSERT")
	if err != nil {
		glog.Infof("Successfully registered %s in Route53", alb.hostname)
	}
	return err
}

func (r *Route53) deleteRecord(alb *albIngress) error {
	err := r.modifyRecord(alb, "DELETE")
	if err != nil {
		glog.Infof("Successfully deleted %s from Route53", alb.hostname)
	}
	return err
}

func (r *Route53) modifyRecord(alb *albIngress, action string) error {
	hostedZone, err := r.getZoneID(alb.hostname)
	if err != nil {
		return err
	}

	// Need check if the record exists and remove it if it does in this changeset
	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String(action),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(alb.hostname),
						Type: aws.String("A"),
						AliasTarget: &route53.AliasTarget{
							DNSName:              alb.elbv2.LoadBalancer.DNSName,
							EvaluateTargetHealth: aws.Bool(false),
							HostedZoneId:         alb.elbv2.CanonicalHostedZoneId,
						},
					},
				},
			},
			Comment: aws.String("Managed by Kubernetes"),
		},
		HostedZoneId: hostedZone.Id, // Required
	}

	resp, err := r.ChangeResourceRecordSets(params)
	if err != nil {
		AWSErrorCount.With(prometheus.Labels{"service": "Route53"}).Add(float64(1))
		glog.Errorf("There was an Error calling Route53 ChangeResourceRecordSets: %+v. Error: %s", resp.GoString(), err.Error())
		return err
	}

	return nil
}

func (r *Route53) sanityTest() {
	glog.Warning("Verifying Route53 connectivity") // TODO: Figure out why we can't see this
	_, err := r.ListHostedZones(&route53.ListHostedZonesInput{MaxItems: aws.String("1")})
	if err != nil {
		panic(err)
	}
	glog.Warning("Verified")
}
