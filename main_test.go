package main

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/stretchr/testify/assert"
)

type listenerDescription struct {
	protocol string
	port     int64
}

type elbBuilder struct {
	loadBalancerName     *string
	listenerDescriptions []*elb.ListenerDescription
	subnets              []*string
	securityGroups       []*string
}

func (b *elbBuilder) withListenerDescriptions(listenerDescriptions ...listenerDescription) *elbBuilder {
	b.listenerDescriptions = make([]*elb.ListenerDescription, 0)

	for i := range listenerDescriptions {
		b.listenerDescriptions = append(b.listenerDescriptions, &elb.ListenerDescription{
			Listener: &elb.Listener{
				LoadBalancerPort: &listenerDescriptions[i].port,
				Protocol:         &listenerDescriptions[i].protocol,
			},
		})
	}

	return b
}

func (b *elbBuilder) withSecurityGroups(securityGroups ...string) *elbBuilder {
	b.securityGroups = make([]*string, 0)
	for i := range securityGroups {
		b.securityGroups = append(b.securityGroups, &securityGroups[i])
	}
	return b
}

func (b *elbBuilder) withSubnets(subnets ...string) *elbBuilder {
	b.subnets = make([]*string, 0)
	for i := range subnets {
		b.subnets = append(b.subnets, &subnets[i])
	}
	return b
}

func (b *elbBuilder) build() *elb.LoadBalancerDescription {
	if b.subnets == nil || len(b.subnets) == 0 {
		panic("ELB must have at least one subnet")
	}

	if b.loadBalancerName == nil {
		panic("ELB must have a LoadBalancerName")
	}

	return &elb.LoadBalancerDescription{
		LoadBalancerName:     b.loadBalancerName,
		Subnets:              b.subnets,
		ListenerDescriptions: b.listenerDescriptions,
		SecurityGroups:       b.securityGroups,
	}
}

func createELB(name string) *elbBuilder {
	return &elbBuilder{
		loadBalancerName: &name,
	}
}

func TestSameSubnetsAreInTheSamePartition(t *testing.T) {
	// ELBs in the same subnet should be in the same partition because they're the same network tier.
	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
		createELB("second").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations), "We have a single recommendation")

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "There is only one subnet")
	assert.Equal(t, 1, len(answer.ALBs()), "There is only one ALB")
	assert.Equal(t, 0, len(answer.NLBs()), "There are no NLBs")

	lb := answer.ALBs()[0]
	assert.Equal(t, 2, len(lb.ELBs()), "There are 2 ELBs being replaced")
	assert.Equal(t, []string{"80", "443"}, lb.Ports())
}

func TestIntersectingSubnetsAreInTheSamePartition(t *testing.T) {
	// An ELB in the subnets [a,b] and an ELB in the subnets [b,c] should be in the same partition
	// because b is common, and by extension, a and c are also in the same one.
	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a", "b").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
		createELB("second").
			withSubnets("b", "c").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations), "We have a single recommendation: %#v", recommendations)

	answer := recommendations[0]
	assert.Equal(t, 3, len(answer.Subnets()), "There are 3 subnets")
	assert.Equal(t, 1, len(answer.ALBs()), "There is only one ALB")
	assert.Equal(t, 0, len(answer.NLBs()), "There are no NLBs")

	lb := answer.ALBs()[0]
	assert.Equal(t, 2, len(lb.ELBs()), "There are 2 ELBs being replaced")
}

func TestDistinctSubnetsAreInDifferentPartitions(t *testing.T) {
	// An ELB in the subnets [a, ... , z] and an ELB in the subnets [A, ... , Z] should be in
	// different partitions
	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			build(),
		createELB("second").
			withSubnets("A").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 2, len(recommendations), "We have 2 recommendations")

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "There is only one subnet")
	assert.Equal(t, 1, len(answer.ALBs()), "There is only one ALB")
	assert.Equal(t, 0, len(answer.NLBs()), "There are no NLBs")

	lb := answer.ALBs()[0]
	assert.Equal(t, 1, len(lb.ELBs()), "There is 1 ELB being replaced")

	answer = recommendations[1]
	assert.Equal(t, 1, len(answer.Subnets()), "There is only one subnet")
	assert.Equal(t, 1, len(answer.ALBs()), "There is only one ALB")
	assert.Equal(t, 0, len(answer.NLBs()), "There are no NLBs")

	lb = answer.ALBs()[0]
	assert.Equal(t, 1, len(lb.ELBs()), "There is 1 ELB being replaced")
}

func TestTheSameSecurityGroupIsEquivalent(t *testing.T) {
	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
		createELB("second").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations))

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "There is only one subnet")
	assert.Equal(t, 1, len(answer.ALBs()), "There is only one ALB")
	assert.Equal(t, 0, len(answer.NLBs()), "There are no NLBs")

	lb := answer.ALBs()[0]
	assert.Equal(t, 2, len(lb.ELBs()), "Number of ELBs being replaced")
}

func TestDifferentSecurityGroupsWithDistinctCidrsAreSeparate(t *testing.T) {
	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
		createELB("second").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-2").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	sgs["sg-1"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-1"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
		},
	}

	sgs["sg-2"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-2"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("192.168.0.1/32"),
					},
				},
			},
		},
	}

	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations))

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "Subnets")
	assert.Equal(t, 2, len(answer.ALBs()), "ALBs")
	assert.Equal(t, 0, len(answer.NLBs()), "NLBs")

	lb := answer.ALBs()[0]
	assert.Equal(t, 1, len(lb.ELBs()), "ELBs")
	assert.Equal(t, []string{"80", "443"}, lb.Ports())
	assert.Equal(t, []string{"sg-1"}, lb.SecurityGroups())

	lb = answer.ALBs()[1]
	assert.Equal(t, 1, len(lb.ELBs()), "ELBs")
	assert.Equal(t, []string{"80", "443"}, lb.Ports())
	assert.Equal(t, []string{"sg-2"}, lb.SecurityGroups())
}

func sPtr(s string) *string {
	return &s
}

func int64Ptr(i int64) *int64 {
	return &i
}

func TestSecurityGroupsWithTheSameSrcCIDRsAreEquivalent(t *testing.T) {
	// Allowing port 443 and port 80 from the same src seem like the same security group. Also
	// allowing port 22 from that src again seems like the same security group. So the key should
	// be a hash of the canonical source CIDRs

	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 80, protocol: "HTTP"},
			).
			withSecurityGroups("sg-1").
			build(),
		createELB("second").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-2").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	sgs["sg-1"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-1"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
		},
	}

	sgs["sg-2"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-2"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(80),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
		},
	}

	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations))

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "Subnets")
	assert.Equal(t, 1, len(answer.ALBs()), "ALBs")
	assert.Equal(t, 0, len(answer.NLBs()), "NLBs")

	lb := answer.ALBs()[0]
	assert.Equal(t, 2, len(lb.ELBs()), "ELBs")
	assert.Equal(t, []string{"80", "443"}, lb.Ports())
	assert.Equal(t, []string{"sg-1", "sg-2"}, lb.SecurityGroups())
}

func TestOverlappingSecurityGroupsAreCoalesced(t *testing.T) {
	// Two security groups:
	//
	// 1. allowing from src [a, b, c, d] to port 443
	// 2. allowing from src [a] to port 443
	//
	// second security group is redundant and can be ignored (but might be retained to enable the
	// fast path with security group name matching?)

	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-1").
			build(),
		createELB("second").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 443, protocol: "HTTPS"},
			).
			withSecurityGroups("sg-2", "sg-3").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	sgs["sg-1"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-1"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.1/32"),
					},
				},
			},
		},
	}

	sgs["sg-2"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-2"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.1/32"),
					},
					{
						CidrIp: sPtr("10.0.0.2/32"),
					},
					{
						CidrIp: sPtr("10.0.0.3/32"),
					},
					{
						CidrIp: sPtr("10.0.0.4/32"),
					},
				},
			},
		},
	}

	sgs["sg-3"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-3"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.1/32"),
					},
				},
			},
		},
	}

	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations))

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "Subnets")
	assert.Equal(t, 1, len(answer.ALBs()), "ALBs")
	assert.Equal(t, 0, len(answer.NLBs()), "NLBs")

	lb := answer.ALBs()[0]
	assert.Equal(t, 2, len(lb.ELBs()), "ELBs")
	assert.Equal(t, []string{"443"}, lb.Ports())
	assert.Equal(t, []string{"sg-1", "sg-2", "sg-3"}, lb.SecurityGroups())
}

func TestSubsetOfSrcIngressToUniquePortIsMeaningfulDistinction(t *testing.T) {
	// Two security groups:
	//
	// 1. allowing from src [a, b, c, d] to port 443
	// 2. allowing from src [a] to port 22
	//
	// second security group is meaningfully different – maybe allowing admin access from a
	// specific egress or network tier?

	sgs := make(map[string]*ec2.SecurityGroup)
	sgs["sg-1"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-1"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(22),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.1/32"),
					},
				},
			},
		},
	}

	sgs["sg-2"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-2"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.1/32"),
					},
					{
						CidrIp: sPtr("10.0.0.2/32"),
					},
					{
						CidrIp: sPtr("10.0.0.3/32"),
					},
					{
						CidrIp: sPtr("10.0.0.4/32"),
					},
				},
			},
		},
	}

	tiers := newTiers(sgs)
	assert.False(t, tiers.hasSameIngress("sg-1", "sg-2"))
}

func Test2ELBsWithPortCollisionBecome2NLBs(t *testing.T) {
	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 10201, protocol: "TCP"},
			).
			withSecurityGroups("sg-1").
			build(),
		createELB("second").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 10201, protocol: "TCP"},
			).
			withSecurityGroups("sg-2").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	sgs["sg-1"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-1"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(10201),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
		},
	}

	sgs["sg-2"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-2"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(10201),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
		},
	}

	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations), "Both ELBs are in the same subnet, so a single tier")

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "Subnets")
	assert.Equal(t, 0, len(answer.ALBs()), "ALBs")
	assert.Equal(t, 2, len(answer.NLBs()), "NLBs")
	assert.Equal(t, 0, len(answer.ELBs()), "ELBs")

	lb := answer.NLBs()[0]
	assert.Equal(t, 1, len(lb.ELBs()), "ELBs")
	assert.Equal(t, "first", lb.ELBs()[0])
	assert.Equal(t, []string{"10201"}, lb.Ports())
	assert.Equal(t, []string{"sg-1"}, lb.SecurityGroups())

	lb = answer.NLBs()[1]
	assert.Equal(t, 1, len(lb.ELBs()), "ELBs")
	assert.Equal(t, "second", lb.ELBs()[0])
	assert.Equal(t, []string{"10201"}, lb.Ports())
	assert.Equal(t, []string{"sg-2"}, lb.SecurityGroups())
}

func TestELBDoingDifferentProtocolsIsRetained(t *testing.T) {
	elbs := []*elb.LoadBalancerDescription{
		createELB("first").
			withSubnets("a").
			withListenerDescriptions(
				listenerDescription{port: 11210, protocol: "TCP"},
				listenerDescription{port: 443, protocol: "HTTPS"},
				listenerDescription{port: 80, protocol: "HTTP"},
			).
			withSecurityGroups("sg-1").
			build(),
	}

	sgs := make(map[string]*ec2.SecurityGroup)
	sgs["sg-1"] = &ec2.SecurityGroup{
		GroupId: sPtr("sg-1"),
		IpPermissions: []*ec2.IpPermission{
			{
				FromPort:   int64Ptr(11210),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
			{
				FromPort:   int64Ptr(443),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
			{
				FromPort:   int64Ptr(80),
				IpProtocol: sPtr("tcp"),
				IpRanges: []*ec2.IpRange{
					{
						CidrIp: sPtr("10.0.0.0/8"),
					},
				},
			},
		},
	}

	recommendations := generateRecommendations(elbs, sgs)

	assert.Equal(t, 1, len(recommendations))

	answer := recommendations[0]
	assert.Equal(t, 1, len(answer.Subnets()), "Subnets")
	assert.Equal(t, 0, len(answer.ALBs()), "ALBs")
	assert.Equal(t, 0, len(answer.NLBs()), "NLBs")
	assert.Equal(t, 1, len(answer.ELBs()), "ELBs")

	lb := answer.ELBs()[0]
	assert.Equal(t, 1, len(lb.ELBs()), "ELBs")
	assert.Equal(t, "first", lb.ELBs()[0])
	assert.Equal(t, []string{"80", "443", "11210"}, lb.Ports())
	assert.Equal(t, []string{"sg-1"}, lb.SecurityGroups())
}

func TestTCPOverPort80IsTreatedAsHTTP(t *testing.T) {
	lb := createELB("first").
		withSubnets("a").
		withListenerDescriptions(
			listenerDescription{port: 80, protocol: "TCP"},
		).
		build()
	assert.Equal(t, ALB, inspectListeners(lb))
}

func TestTCPOverPort443IsTreatedAsHTTPS(t *testing.T) {
	lb := createELB("first").
		withSubnets("a").
		withListenerDescriptions(
			listenerDescription{port: 443, protocol: "TCP"},
		).
		build()
	assert.Equal(t, ALB, inspectListeners(lb))
}

func TestTCPOverPort8080IsTreatedAsTCP(t *testing.T) {
	lb := createELB("first").
		withSubnets("a").
		withListenerDescriptions(
			listenerDescription{port: 8080, protocol: "TCP"},
		).
		build()
	assert.Equal(t, NLB, inspectListeners(lb))
}
