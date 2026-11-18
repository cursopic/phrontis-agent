package pricing

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// SpotAPI is the one EC2 call spot prices need (ec2.Client has it); tests pass a fake. Its IAM policy needs only
// ec2:DescribeSpotPriceHistory.
type SpotAPI = ec2.DescribeSpotPriceHistoryAPIClient

// SpotTTL is how long a looked-up spot price is reused (spec 011).
const SpotTTL = time.Hour

// spotFailTTL is how long a failed lookup is reused, so an AWS outage costs one call per type, not one per report.
const spotFailTTL = 5 * time.Minute

// spotFailed is the only reason a failed call gives: AWS errors carry our IAM ARN and account id, and reasons reach
// every tenant's report. The error itself goes to the log only.
const spotFailed = "spot price lookup failed"

// spotTimeout bounds one lookup, so a stalled AWS call cannot eat the report's 25 s.
const spotTimeout = 10 * time.Second

// SpotPrice is one instance type's spot price in a region. With Reason set there is no price: Hourly is 0.
type SpotPrice struct {
	Hourly float64
	At     time.Time // UTC; the price is the one in effect at this time
	Reason string
}

// Label is how a priced spot row is shown: "spot, price at <time>".
func (s SpotPrice) Label() string {
	return "spot (highest zone), price at " + s.At.Format("2006-01-02 15:04 UTC")
}

type spotCache struct {
	p       SpotPrice
	expires time.Time
}

// NewSpotAPI builds the EC2 client from the standard AWS credential chain (env, shared files, instance role). The key
// stays inside the SDK: Phrontis never reads, logs or stores it. It fails when no credentials resolve.
func NewSpotAPI(ctx context.Context) (SpotAPI, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1")) // each call sets the node's region
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("aws credentials for spot prices: %w", err)
	}
	return ec2.NewFromConfig(cfg), nil
}

// SpotHourly returns the Linux spot price of k's type in k's region now, from DescribeSpotPriceHistory, reusing a
// lookup for SpotTTL. Several zones: the highest zone price, so savings are never overstated. A failed call is reused
// for spotFailTTL.
func (c *Client) SpotHourly(ctx context.Context, k Key) SpotPrice {
	if c.Spot == nil {
		return SpotPrice{Reason: "no AWS spot price source configured"}
	}
	if !regionRe.MatchString(k.Region) || !typeRe.MatchString(k.Type) {
		return SpotPrice{Reason: fmt.Sprintf("refusing odd region %q or type %q", k.Region, k.Type)}
	}
	now := c.now().UTC()
	ck := k.Region + "/" + k.Type
	c.spotMu.Lock()
	defer c.spotMu.Unlock()
	if e, ok := c.spot[ck]; ok && now.Before(e.expires) {
		return e.p
	}
	p, err := c.fetchSpot(ctx, k, now)
	ttl := SpotTTL
	if err != nil {
		slog.Warn(spotFailed, "region", k.Region, "type", k.Type, "err", err)
		p, ttl = SpotPrice{Reason: spotFailed}, spotFailTTL
	}
	if c.spot == nil {
		c.spot = map[string]spotCache{}
	}
	c.spot[ck] = spotCache{p, now.Add(ttl)}
	return p
}

func (c *Client) fetchSpot(ctx context.Context, k Key, now time.Time) (SpotPrice, error) {
	ctx, cancel := context.WithTimeout(ctx, spotTimeout)
	defer cancel()
	// Start = End = now: AWS returns the last change before the start, i.e. the price in effect now, per zone.
	in := &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(k.Type)},
		ProductDescriptions: []string{"Linux/UNIX"},
		StartTime:           aws.Time(now),
		EndTime:             aws.Time(now),
	}
	region := func(o *ec2.Options) { o.Region = k.Region }
	best := -1.0
	for pg := ec2.NewDescribeSpotPriceHistoryPaginator(c.Spot, in); pg.HasMorePages(); {
		out, err := pg.NextPage(ctx, region)
		if err != nil {
			return SpotPrice{}, err
		}
		for _, s := range out.SpotPriceHistory {
			if s.SpotPrice == nil {
				continue
			}
			if v, err := strconv.ParseFloat(*s.SpotPrice, 64); err == nil && v > 0 && v > best {
				best = v
			}
		}
	}
	if best < 0 {
		return SpotPrice{At: now, Reason: fmt.Sprintf("AWS lists no Linux spot price for %s in %s", k.Type, k.Region)}, nil
	}
	return SpotPrice{Hourly: best, At: now}, nil
}
