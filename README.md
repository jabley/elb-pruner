![Go](https://github.com/jabley/elb-pruner/workflows/Go/badge.svg?branch=master)

ALBs and NLBs are ~10% cheaper than a classic ELB. If you can use the former instead, you'll probably save money.

But more savings might be available if you can replace many load balancers with a single ALB which does host-based routing to send to the appropriate Auto-Scaling Group.

Frequently I see an ASG with its own Load Balancer. That might make sense. If a Load Balancer has a particular Security Group which only permits certain clients to talk to it, that can be a good security configuration.

But if the security groups and subnets are the same across more than one Load Balancer, then you can run a cheaper configuration.

## Installation and use

`go get github.com/jabley/elb-pruner`

or

`git clone github.com/jabley/elb-pruner && cd elb-pruner && go build`

`./elb-pruner -profile {AWS_PROFILE_NAME}` will use normal AWS access controls to read your account and suggestion optimisations.

**It does not modify anything**.

It's quite aggressive. It will try to create the minimum number of ALBs, NLBs and ELBs for each network tier. That might not be what you want, but it will hopefully make you think hard about what you do need, and make active decisions around security and cost.

I've used this to evaluate changes which meant an 80% reduction in deployed ELBs, and a reasonable saving per month.

## Limitations

* Untested with different VPCs – I tend to have a single VPC in an account. It might still work, because presumably the subnet names are different across VPCs?
