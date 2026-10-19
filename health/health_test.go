package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/gofiber/fiber/v3"
)

func ok(context.Context) error   { return nil }
func boom(context.Context) error { return errors.New("dial tcp 10.0.0.7:6379: refused") }

type fakeDB struct{ err error }

func (f fakeDB) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, f.err
}

func get(t *testing.T, h fiber.Handler) (int, string, string) {
	t.Helper()
	app := fiber.New()
	app.Get("/", h)
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestReportStatusMatrix(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		code   int
		status string
	}{
		{"no checks", nil, 200, StatusPass},
		{"all pass", []Check{DynamoDB(fakeDB{}, "t", true), Cache(ok, false)}, 200, StatusPass},
		{"primary fails", []Check{DynamoDB(fakeDB{err: errors.New("x")}, "t", true)}, 503, StatusFail},
		{"secondary fails only warns", []Check{DynamoDB(fakeDB{}, "t", true), Cache(boom, false), JWKS(boom, false)}, 207, StatusWarn},
		{"required cache fails", []Check{Cache(boom, true)}, 503, StatusFail},
		{"nil cache secondary warns", []Check{Cache(nil, false)}, 207, StatusWarn},
		{"nil cache primary fails", []Check{Cache(nil, true)}, 503, StatusFail},
		{"nil db fails when primary", []Check{DynamoDB(nil, "t", true)}, 503, StatusFail},
		{"high utilization warns", []Check{{Name: "cpu", Probe: func(context.Context) (Observation, error) {
			return Observation{Value: 99, Warn: true}, nil
		}}}, 207, StatusWarn},
		{"panic is contained", []Check{{Name: "p", Probe: func(context.Context) (Observation, error) { panic("x") }}}, 207, StatusWarn},
		{"nil probe primary", []Check{{Name: "n", Primary: true}}, 503, StatusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := New(Config{ServiceID: "svc", ReleaseID: "r1", Version: "/v1"}).Add(tc.checks...)
			code, ct, body := get(t, h.Report())
			if code != tc.code || ct != ContentType {
				t.Fatalf("code=%d ct=%q body=%s", code, ct, body)
			}
			var r Report
			if err := json.Unmarshal([]byte(body), &r); err != nil {
				t.Fatal(err)
			}
			if r.Status != tc.status || r.ServiceID != "svc" || r.Checks[MeasureUptime].Status != StatusPass {
				t.Errorf("report = %+v", r)
			}
			if strings.Contains(body, "10.0.0.7") {
				t.Errorf("dependency error text leaked: %s", body)
			}
		})
	}
}

func TestPrimaryFailBeatsWarn(t *testing.T) {
	h := New(Config{}).Add(Cache(boom, false), Cache(boom, true))
	h.checks[1].Name = "other"
	_, code := h.Run(context.Background())
	if code != 503 {
		t.Fatalf("code = %d", code)
	}
}

func TestCheckTimeoutIsPerCheck(t *testing.T) {
	slow := Check{Name: "slow", Primary: true, Probe: func(ctx context.Context) (Observation, error) {
		<-ctx.Done()
		return Observation{}, ctx.Err()
	}}
	h := New(Config{Timeout: 50 * time.Millisecond}).Add(slow)
	start := time.Now()
	_, code := h.Run(context.Background())
	if code != 503 || time.Since(start) > time.Second {
		t.Fatalf("code=%d elapsed=%s", code, time.Since(start))
	}
}

func TestLivenessHasNoDependencies(t *testing.T) {
	h := New(Config{ServiceID: "svc", ReleaseID: "r1"}).Add(Cache(boom, true))
	code, ct, body := get(t, h.Liveness())
	if code != 200 || ct != ContentType || !strings.Contains(body, `"status":"pass"`) {
		t.Fatalf("code=%d ct=%q body=%s", code, ct, body)
	}
}

func TestBuiltinSystemChecksNeverFail(t *testing.T) {
	for _, c := range []Check{CPU(), Memory()} {
		if c.Primary {
			t.Errorf("%s must not be primary", c.Name)
		}
		obs, err := c.Probe(context.Background())
		if err != nil || obs.Unit != UnitPercent || obs.Value > 100 {
			t.Errorf("%s: %+v %v", c.Name, obs, err)
		}
	}
}

func TestRoundOneKeepsSentinel(t *testing.T) {
	if roundOne(Unavailable) != Unavailable || roundOne(12.34) != 12.3 {
		t.Fatal("roundOne")
	}
}
