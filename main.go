package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
)

type lbType int

const (
	// ALB is an Application Load Balancer that only speaks HTTP(S)
	ALB lbType = iota
	// NLB is a Network Load Balancer that only speaks TCP (and UDP?)
	NLB
	// ELB is a classic LoadBalancer
	ELB
)

type arguments struct {
	profile string
}

// tier is a set of one or more subnets. In an AWS account, we might have a:
//
// - public subnet
// - app subnet
// - private subnet
// - database subnet
// - etc
type tier struct {
	subnets        map[string]struct{}
	recommendation *recommendation
}

func (t *tier) add(subnet *string) {
	t.subnets[*subnet] = struct{}{}
}

func (t *tier) keys() []string {
	keys := make([]string, len(t.subnets))

	i := 0
	for k := range t.subnets {
		keys[i] = k
		i++
	}

	sort.Strings(keys)

	return keys
}

// tiers is a holder for all of the tiers we've discovered. It also contains caches for comparisons.
type tiers struct {
	tiersBySubnet  map[string]*tier              // tiers keyed by subnet name
	tiers          []*tier                       // the list of tiers
	securityGroups map[string]*ec2.SecurityGroup // security groups keyed by GroupId
	ingressesBySg  map[string]map[string]bool    // set of ingress CIDRs keyed by Security Group GroupId
}

// newTiers creates a new tiers struct ready for use
func newTiers(sgs map[string]*ec2.SecurityGroup) *tiers {
	return &tiers{
		tiersBySubnet:  make(map[string]*tier),
		tiers:          make([]*tier, 0),
		securityGroups: sgs,
		ingressesBySg:  make(map[string]map[string]bool),
	}
}

func (t *tiers) addTierFor(subnet *string) *tier {
	res := &tier{
		subnets:        make(map[string]struct{}),
		recommendation: newRecommendation(),
	}
	t.associate(res, subnet)
	t.tiers = append(t.tiers, res)

	return res
}

func (t *tiers) associate(tier *tier, subnet *string) {
	tier.add(subnet)
	t.tiersBySubnet[*subnet] = tier
}

func (t *tiers) find(subnet *string) *tier {
	if res, ok := t.tiersBySubnet[*subnet]; ok {
		return res
	}

	return t.addTierFor(subnet)
}

func (t *tiers) findOrGetIngress(sg string) map[string]bool {
	if res, ok := t.ingressesBySg[sg]; ok {
		return res
	}

	res := make(map[string]bool)

	for _, permission := range t.securityGroups[sg].IpPermissions {
		for _, cidr := range permission.IpRanges {
			res[*cidr.CidrIp] = true
		}
	}

	t.ingressesBySg[sg] = res

	return res
}

// hasSameIngress is an equality test between 2 security groups. Ingress CIDRs need to be
// identical. We don't consider set operations in terms of one ingress is a proper subset of
// another. Equality only at this time.
func (t *tiers) hasSameIngress(sg1, sg2 string) bool {
	ingress1 := t.findOrGetIngress(sg1)
	ingress2 := t.findOrGetIngress(sg2)

	return reflect.DeepEqual(ingress1, ingress2)
}

func (t *tiers) recommendations() []recommendation {
	result := make([]recommendation, 0)

	// TODO(jabley): this is little messy – fix data structures!
	for _, tier := range t.tiers {
		tier.recommendation.subnets = tier.keys()
		result = append(result, *tier.recommendation)
	}

	return result
}

// recommendation is a summary of how we might restructure the ELBs in the account for a given tier.
type recommendation struct {
	subnets  []string       // the set of subnets that this recommendation covers
	albs     []*LB          // the non-nil ALBs that should live in the subnets
	albsBySg map[string]*LB // the ALBs keyed by Security Group GroupId
	nlbs     []*LB          // the non-nil NLBs that should live in the subnets
	nlbsBySg map[string]*LB // the NLBs keyed by Security Group GroupId
	elbs     []*LB          // the non-nil ELBs that should live in the subnets
	elbsBySg map[string]*LB // the ELBs keyed by Security Group GroupId
}

// newRecommendation creates a new recommendation instance ready for use
func newRecommendation() *recommendation {
	return &recommendation{
		albs:     make([]*LB, 0),
		albsBySg: make(map[string]*LB),
		nlbs:     make([]*LB, 0),
		nlbsBySg: make(map[string]*LB),
		elbs:     make([]*LB, 0),
		elbsBySg: make(map[string]*LB),
	}
}

// associateALBWithSecurityGroups tracks that we consider this ALB to be suitable for the provided
// security groups.
func (r *recommendation) associateALBWithSecurityGroups(alb *LB, securityGroups []*string) {
	for i := range securityGroups {
		r.albsBySg[*securityGroups[i]] = alb
	}
}

func (r *recommendation) ALBs() []*LB {
	return r.albs
}

// associateNLBWithSecurityGroups tracks that we consider this NLB to be suitable for the provided
// security groups.
func (r *recommendation) associateNLBWithSecurityGroups(nlb *LB, securityGroups []*string) {
	for i := range securityGroups {
		r.nlbsBySg[*securityGroups[i]] = nlb
	}
}

func (r *recommendation) NLBs() []*LB {
	return r.nlbs
}

// associateELBWithSecurityGroups tracks that we consider this ELB to be suitable for the provided
// security groups.
func (r *recommendation) associateELBWithSecurityGroups(elb *LB, securityGroups []*string) {
	for i := range securityGroups {
		r.elbsBySg[*securityGroups[i]] = elb
	}
}

func (r *recommendation) ELBs() []*LB {
	return r.elbs
}

func (r *recommendation) Subnets() []string {
	return r.subnets
}

// LB is an ALB or NLB that can replace one or more ELBs
type LB struct {
	elbs           []string            // the names of the ELBs that this LB can replace
	ports          map[int]struct{}    // the set of ports that this LB will listen on
	securityGroups map[string]struct{} // the set of Security Groups that this LB will allow
}

// newLB creates a new LB ready for use. It will expose the listener ports of the provided non-nil
// ELB, and the same Security Groups.
func newLB(elb *elb.LoadBalancerDescription) *LB {
	res := &LB{
		elbs:           []string{},
		ports:          make(map[int]struct{}),
		securityGroups: make(map[string]struct{}),
	}

	res.replaceELB(elb)

	return res
}

// replaceELB adds the specified ELB to the set of ELBs that this LB can replace. It will expose
// the same listener ports and use the same Security Groups.
func (lb *LB) replaceELB(elb *elb.LoadBalancerDescription) {
	lb.elbs = append(lb.elbs, *elb.LoadBalancerName)
	lb.addPorts(listenerPorts(elb.ListenerDescriptions))
	lb.addSecurityGroups(elb.SecurityGroups)
}

func (lb *LB) addPorts(ports []int) {
	for i := range ports {
		if _, ok := lb.ports[ports[i]]; !ok {
			lb.ports[ports[i]] = struct{}{}
		}
	}
}

// hasPortCollision returns true if the specified ELB has any listening ports matching ports
// already assigned by this LB
func (lb *LB) hasPortCollision(elb *elb.LoadBalancerDescription) bool {
	for i := range elb.ListenerDescriptions {
		if _, ok := lb.ports[int(*elb.ListenerDescriptions[i].Listener.LoadBalancerPort)]; ok {
			return true
		}
	}
	return false
}

func (lb *LB) addSecurityGroups(securityGroups []*string) {
	for i := range securityGroups {
		if _, ok := lb.securityGroups[*securityGroups[i]]; !ok {
			lb.securityGroups[*securityGroups[i]] = struct{}{}
		}
	}
}

// ELBs returns the non-nil array of ELB names that can be replaced by this LB
func (lb *LB) ELBs() []string {
	return lb.elbs
}

// Ports returns the non-nil array of ports that the ALB should listen on
func (lb *LB) Ports() []string {
	res := make([]int, 0)
	for k := range lb.ports {
		res = append(res, k)
	}

	// We sort the ports in ascending order, because that seems like a reasonable expectation
	sort.Ints(res)

	buf := make([]string, len(res))
	for i := range res {
		buf[i] = strconv.Itoa(res[i])
	}
	return buf
}

// SecurityGroups returns the non-nil array of security groups names that the ALB should have attached
func (lb *LB) SecurityGroups() []string {
	res := make([]string, 0)

	for k := range lb.securityGroups {
		res = append(res, k)
	}

	// We sort the security group names because that seems like a reasonable expectation
	sort.Strings(res)

	return res
}

func listenerPorts(listeners []*elb.ListenerDescription) []int {
	result := make([]int, 0)

	for i := range listeners {
		result = append(result, int(*listeners[i].Listener.LoadBalancerPort))
	}

	return result
}

func generateRecommendations(elbs []*elb.LoadBalancerDescription, sgs map[string]*ec2.SecurityGroup) []recommendation {
	// for lb in elbs
	//   assign the tier
	//   assign the candidate type
	//     can it be an ALB
	//       does it only speak HTTP(S), or TCP on port 80/443
	//     can it be an NLB
	//       does it only speak TCP
	//     can it be a shared ELB
	//       does it speak both TCP and HTTP(S)
	//    find the type with the equivalent security group

	tiers := newTiers(sgs)

	for _, lb := range elbs {
		elbDrop(tiers, lb)
	}

	return tiers.recommendations()
}

// elbDrop is modelled after a penny fall machine that you might see at an arcade.
//
// 1. The first level assesses which subnets the ELB is in.
// 2. The second level decides which type of LB might replace the ELB
// 3. The third level looks at the security groups and see if an existing replacement has the same
//    security groups
func elbDrop(tiers *tiers, lb *elb.LoadBalancerDescription) {
	recommendation := assignTier(tiers, lb)
	targetLB := inspectListeners(lb)
	switch targetLB {
	case ALB:
		addELBv2(lb, tiers,
			len(recommendation.albs) == 0,
			true, // ALBs can do port collisions - we can do host-based routing to select a backend
			func(alb *LB) {
				recommendation.albs = append(recommendation.albs, alb)
			},
			func(alb *LB, securityGroups []*string) {
				recommendation.associateALBWithSecurityGroups(alb, securityGroups)
			},
			recommendation.albsBySg,
		)
	case NLB:
		addELBv2(lb, tiers,
			len(recommendation.nlbs) == 0,
			false, // NLBs can't do port collisions - no routing options to decide on a backend?
			func(nlb *LB) {
				recommendation.nlbs = append(recommendation.nlbs, nlb)
			}, func(nlb *LB, securityGroups []*string) {
				recommendation.associateNLBWithSecurityGroups(nlb, securityGroups)
			},
			recommendation.nlbsBySg,
		)
	case ELB:
		addELBv2(lb, tiers,
			len(recommendation.elbs) == 0,
			false, // ELBs can't do port collisions - no routing options to decide on a backend?
			func(elb *LB) {
				recommendation.elbs = append(recommendation.elbs, elb)
			},
			func(elb *LB, securityGroups []*string) {
				recommendation.associateELBWithSecurityGroups(elb, securityGroups)
			},
			recommendation.elbsBySg,
		)
	default:
		panic("Uknown type of LB")
	}
}

func addELBv2(
	lb *elb.LoadBalancerDescription,
	tiers *tiers,
	firstELBv2 bool,
	allowPortCollisions bool,
	add func(*LB),
	associate func(*LB, []*string),
	existingELBv2sBySg map[string]*LB,
) {

	if firstELBv2 {
		elbv2 := newLB(lb)

		add(elbv2)
		associate(elbv2, lb.SecurityGroups)

		return
	}

	for _, lbSecurityGroup := range lb.SecurityGroups {
		// do we have an existing one with this security group?
		elbv2, ok := existingELBv2sBySg[*lbSecurityGroup]
		if ok && (allowPortCollisions || !elbv2.hasPortCollision(lb)) {
			associate(elbv2, lb.SecurityGroups)
			elbv2.replaceELB(lb)
			return
		}

		// Have we already processed an SG which has the same ingress?
		for seenSg := range existingELBv2sBySg {
			if tiers.hasSameIngress(seenSg, *lbSecurityGroup) {
				elbv2 := existingELBv2sBySg[seenSg]
				if allowPortCollisions || !elbv2.hasPortCollision(lb) {
					associate(elbv2, lb.SecurityGroups)
					elbv2.replaceELB(lb)
					return
				}
			}
		}
	}

	// Distinctly new SecurityGroup – a new ELBv2 then
	elbv2 := newLB(lb)
	add(elbv2)
	associate(elbv2, lb.SecurityGroups)
}

func inspectListeners(lb *elb.LoadBalancerDescription) lbType {
	protocols := make(map[string]struct{})

	for _, ld := range lb.ListenerDescriptions {
		switch *ld.Listener.Protocol {
		case "HTTP", "HTTPS":
			protocols["HTTP"] = struct{}{}
		case "TCP":
			if *ld.Listener.LoadBalancerPort == 80 || *ld.Listener.LoadBalancerPort == 443 {
				protocols["HTTP"] = struct{}{}
			} else {
				protocols["TCP"] = struct{}{}
			}
		}
	}

	switch len(protocols) {
	case 0:
		panic("No known protocols for this listener")
	case 1:
		if _, ok := protocols["HTTP"]; ok {
			return ALB
		}
		return NLB
	default:
		return ELB
	}
}

func assignTier(tiers *tiers, lb *elb.LoadBalancerDescription) *recommendation {
	var t *tier

	for _, s := range lb.Subnets {
		if t != nil {
			tiers.associate(t, s)
		} else {
			t = tiers.find(s)
		}
	}

	return t.recommendation
}

func main() {
	args := parseAndVerifyArgs()

	options := session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}

	if args.profile != "" {
		options.Profile = args.profile
	}

	start := time.Now()

	sess := session.Must(session.NewSessionWithOptions(options))

	// Do retries in case we hit the API too hard and get throttled for exceeding our allowed rate.
	elbSvc := elb.New(sess, aws.NewConfig().WithMaxRetries(3))
	ec2Svc := ec2.New(sess, aws.NewConfig().WithMaxRetries(3))

	input := &elb.DescribeLoadBalancersInput{}
	elbs := make([]*elb.LoadBalancerDescription, 0)

	elbSvc.DescribeLoadBalancersPages(input, func(page *elb.DescribeLoadBalancersOutput, lastPage bool) bool {
		elbs = append(elbs, page.LoadBalancerDescriptions...)
		return !lastPage
	})

	sgs := make(map[string]*ec2.SecurityGroup)

	for _, lb := range elbs {
		if lb.SecurityGroups == nil {
			continue
		}

		for _, sg := range lb.SecurityGroups {
			if _, ok := sgs[*sg]; ok {
				continue
			}
			result, err := ec2Svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
				GroupIds: []*string{
					aws.String(*sg),
				},
			})
			panicOnAwsError(err)
			sgs[*sg] = result.SecurityGroups[0]
		}
	}

	fmt.Printf("Read AWS account in %v, generating recommendations...\n\n", time.Since(start))

	recommendations := generateRecommendations(elbs, sgs)

	printRecommendations(recommendations)
}

func printRecommendations(recommendations []recommendation) {
	currentElbCount, albCount, nlbCount, elbCount := 0, 0, 0, 0
	for _, r := range recommendations {
		fmt.Printf("The subnets \"%s\" could contain the following load balancer(s):\n", strings.Join(r.Subnets(), ", "))

		sum := count(r.ALBs())
		currentElbCount += sum.elbs
		albCount += sum.lbs
		printRecommendationFor(r.ALBs(), "ALB")
		println()

		sum = count(r.NLBs())
		currentElbCount += sum.elbs
		nlbCount += sum.lbs
		printRecommendationFor(r.NLBs(), "NLB")
		println()

		sum = count(r.ELBs())
		currentElbCount += sum.elbs
		elbCount += sum.lbs
		printRecommendationFor(r.ELBs(), "ELB")
		println()
	}

	fmt.Printf("So %d ELBs would become %d ALBs, %d NLBs and %d ELBs\n"+
		"with a potential saving of %0.0f%%\n", currentElbCount, albCount, nlbCount, elbCount,
		saving(currentElbCount, albCount, nlbCount, elbCount))
}

type sum struct {
	elbs, lbs int
}

func count(lbs []*LB) *sum {
	res := &sum{
		lbs: len(lbs),
	}
	for i := range lbs {
		res.elbs += len(lbs[i].ELBs())
	}
	return res
}

func saving(current, alb, nlb, elb int) float64 {
	return (float64(current) - ((float64(alb)+float64(nlb))*0.9 + float64(elb))) / float64(current) * 100
}

func printRecommendationFor(lbs []*LB, lbType string) {
	for _, lb := range lbs {
		var action string
		if len(lb.ELBs()) == 1 && lbType == "ELB" {
			action = "Retaining"
		} else {
			action = "Replacing"
		}
		fmt.Printf("\n%s the following load balancers:\n- %s\n\n -> an %s with security groups:\n\t- %s\nexposing the ports:\n\t- %s\n",
			action,
			strings.Join(lb.ELBs(), "\n- "),
			lbType,
			strings.Join(lb.SecurityGroups(), "\n\t- "),
			strings.Join(lb.Ports(), "\n\t- "))
	}
}

func parseAndVerifyArgs() *arguments {
	var (
		help bool
	)

	res := &arguments{}

	flag.BoolVar(&help, "help", false, "Display this help message")
	flag.StringVar(&res.profile, "profile", "", "The AWS profile name to use")

	flag.Usage = func() {
		basename := filepath.Base(os.Args[0])
		fmt.Printf("Usage: %s\n", basename)
		fmt.Printf("A utility to examine ELB usage in an AWS account and recommend ways of consolidating ELBs into ALBs and NLBs")
		flag.PrintDefaults()
	}

	flag.Parse()

	if help {
		flag.Usage()
		os.Exit(1)
	}

	return res
}

func panicOnAwsError(err error) {
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			fmt.Println(err.Error())
		}
		panic("Oh noes!")
	}
}
