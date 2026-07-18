package awsconfig

import (
	"context"
	"testing"
)

func TestLoadSetsRegion(t *testing.T) {
	cfg, err := Load(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != "us-east-1" {
		t.Errorf("Region = %q, want %q", cfg.Region, "us-east-1")
	}
}

func TestNewDynamoDBClientNoOverride(t *testing.T) {
	cfg, err := Load(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	client := NewDynamoDBClient(cfg, "")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewDynamoDBClientWithOverride(t *testing.T) {
	cfg, err := Load(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	client := NewDynamoDBClient(cfg, "http://localhost:8000")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}
