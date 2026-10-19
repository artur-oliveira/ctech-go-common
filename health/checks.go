package health

import (
	"bufio"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// Names, types and units of the built-in checks.
const (
	NameDynamoDB = "dynamodb"
	NameCache    = "cache"
	NameJWKS     = "jwks"
	NameCPU      = "cpu"
	NameMemory   = "memory"

	TypeDatastore      = "datastore"
	TypeDatastoreCache = "datastore:cache"
	TypeComponent      = "component"

	MeasureResponse    = "responseTime"
	MeasureUtilization = "utilization"
	UnitMillisecond    = "millisecond"
	UnitPercent        = "percent"
)

// UtilizationWarnPercent is the CPU/memory level above which a check warns.
const UtilizationWarnPercent = 90

// cpuSampleGap separates the two /proc/stat reads of the CPU check; a single
// read is the since-boot average, which hides a saturated instance (ctech-account
// BUG-040).
const cpuSampleGap = 150 * time.Millisecond

// Procfs paths read by the CPU and memory checks.
const (
	procStat    = "/proc/stat"
	procMeminfo = "/proc/meminfo"
)

// ErrNotConfigured is the probe error of a check whose dependency is nil.
var ErrNotConfigured = errors.New("health: dependency not configured")

// TableDescriber is the slice of *dynamodb.Client the DynamoDB check needs.
type TableDescriber interface {
	DescribeTable(ctx context.Context, in *dynamodb.DescribeTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

// DynamoDB checks one load-bearing table with DescribeTable: resource-scoped,
// so it proves connectivity and that the table exists under the configured
// prefix, with no account-wide IAM. A nil db is ErrNotConfigured.
func DynamoDB(db TableDescriber, table string, primary bool) Check {
	return PingCheck(NameDynamoDB, TypeDatastore, primary, func(ctx context.Context) error {
		if db == nil || table == "" {
			return ErrNotConfigured
		}
		_, err := db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
		return err
	})
}

// Cache checks a cache backend's Ping (cache.Backend satisfies it). Pass
// primary=true only where an unreachable cache is a functional outage (account:
// OAuth codes, MFA); pass a nil ping for an in-memory deployment and it warns.
func Cache(ping func(ctx context.Context) error, primary bool) Check {
	return PingCheck(NameCache, TypeDatastoreCache, primary, ping)
}

// JWKS checks the key set source, e.g. jwtverify.Verifier.Ping.
func JWKS(ping func(ctx context.Context) error, primary bool) Check {
	return PingCheck(NameJWKS, TypeComponent, primary, ping)
}

// PingCheck turns any func(ctx) error into a responseTime check. A nil ping is
// ErrNotConfigured.
func PingCheck(name, componentType string, primary bool, ping func(ctx context.Context) error) Check {
	return Check{Name: name, Type: componentType, Primary: primary, Probe: func(ctx context.Context) (Observation, error) {
		if ping == nil {
			return Observation{}, ErrNotConfigured
		}
		started := time.Now()
		err := ping(ctx)
		return Observation{Measurement: MeasureResponse, Unit: UnitMillisecond,
			Value: float64(time.Since(started).Milliseconds())}, err
	}}
}

// CPU reports utilisation over a short sample window. Never primary: it warns
// above UtilizationWarnPercent or when unmeasurable ("could not tell" is not
// "fine"), because failing it would make the balancer replace a busy instance.
func CPU() Check { return utilization(NameCPU, cpuPercent) }

// Memory reports 1 - MemAvailable/MemTotal (MemFree excludes the page cache,
// which makes a healthy host read 95% used). Never primary, like CPU.
func Memory() Check {
	return utilization(NameMemory, func(context.Context) float64 { return memoryPercent() })
}

func utilization(name string, measure func(context.Context) float64) Check {
	return Check{Name: name, Type: TypeSystem, Probe: func(ctx context.Context) (Observation, error) {
		pct := measure(ctx)
		return Observation{Measurement: MeasureUtilization, Unit: UnitPercent, Value: pct,
			Warn: pct < 0 || pct > UtilizationWarnPercent}, nil
	}}
}

func cpuSample() (idle, total int64, ok bool) {
	f, err := os.Open(procStat)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var vals []int64
	for _, s := range fields[1:] {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			break
		}
		vals = append(vals, v)
		total += v
	}
	if len(vals) < 4 || total == 0 {
		return 0, 0, false
	}
	return vals[3], total, true
}

func cpuPercent(ctx context.Context) float64 {
	if runtime.GOOS != "linux" {
		return Unavailable
	}
	idle1, total1, ok := cpuSample()
	if !ok {
		return Unavailable
	}
	select {
	case <-time.After(cpuSampleGap):
	case <-ctx.Done():
		return Unavailable
	}
	idle2, total2, ok := cpuSample()
	if !ok || total2 <= total1 {
		return Unavailable
	}
	return roundOne(100 * float64((total2-total1)-(idle2-idle1)) / float64(total2-total1))
}

func memoryPercent() float64 {
	if runtime.GOOS != "linux" {
		return Unavailable
	}
	f, err := os.Open(procMeminfo)
	if err != nil {
		return Unavailable
	}
	defer func() { _ = f.Close() }()
	info := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			info[strings.TrimSpace(key)] = v
		}
	}
	total, ok1 := info["MemTotal"]
	avail, ok2 := info["MemAvailable"]
	if !ok1 || !ok2 || total == 0 {
		return Unavailable
	}
	return roundOne(100 * float64(total-avail) / float64(total))
}

// roundOne keeps negative sentinels intact (rounding turned -1 into -0.9 in
// ctech-account BUG-041).
func roundOne(v float64) float64 {
	if v < 0 {
		return v
	}
	return float64(int64(v*10+0.5)) / 10
}
