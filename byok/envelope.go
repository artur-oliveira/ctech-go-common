package byok

import (
	"encoding/json"
	"time"
)

// Op names an outbound Lambda operation (spec § 4.1).
type Op string

const (
	OpConnect         Op = "Connect"
	OpReseal          Op = "Reseal"
	OpToken           Op = "Token"
	OpBalance         Op = "Balance"
	OpStatementPage   Op = "StatementPage"
	OpCreateDueCharge Op = "CreateDueCharge"
	OpGetCharge       Op = "GetCharge"
	OpUpdateCharge    Op = "UpdateCharge"
	OpCancelCharge    Op = "CancelCharge"
	OpGetPayment      Op = "GetPayment"
	OpRefund          Op = "Refund"
	OpGetRefund       Op = "GetRefund"
	OpGetWebhook      Op = "GetWebhook"
	OpRegisterWebhook Op = "RegisterWebhook"
	OpDeleteWebhook   Op = "DeleteWebhook"
	OpListCallbacks   Op = "ListCallbacks"
	OpRetryCallbacks  Op = "RetryCallbacks"
	OpSimulatePayment Op = "SimulatePayment"
)

var AllOps = []Op{OpConnect, OpReseal, OpToken, OpBalance, OpStatementPage, OpCreateDueCharge, OpGetCharge,
	OpUpdateCharge, OpCancelCharge, OpGetPayment, OpRefund, OpGetRefund, OpGetWebhook, OpRegisterWebhook,
	OpDeleteWebhook, OpListCallbacks, OpRetryCallbacks, OpSimulatePayment}

// OpGroup is the allowlist class of an op (spec § 4.1).
type OpGroup string

const (
	GroupRead     OpGroup = "read"
	GroupTeardown OpGroup = "teardown"
	GroupCollect  OpGroup = "collect"
	GroupSandbox  OpGroup = "sandbox"
)

func GroupOf(op Op) OpGroup {
	switch op {
	case OpConnect, OpReseal, OpToken, OpBalance, OpStatementPage, OpGetCharge, OpGetPayment, OpGetRefund,
		OpGetWebhook, OpListCallbacks, OpRetryCallbacks:
		return GroupRead
	case OpCancelCharge, OpDeleteWebhook:
		return GroupTeardown
	case OpCreateDueCharge, OpUpdateCharge, OpRefund, OpRegisterWebhook:
		return GroupCollect
	case OpSimulatePayment:
		return GroupSandbox
	}
	return ""
}

// Request is the outbound Lambda's input. Mode is "live" or "test".
type Request struct {
	Op         Op              `json:"op"`
	Provider   string          `json:"provider"`
	Mode       string          `json:"mode"`
	Connection ConnectionRef   `json:"connection"`
	OAuthToken string          `json:"oauth_token,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Response is the outbound Lambda's output. Error and Code are set together;
// a provider or gate failure is never a Lambda invoke error.
type Response struct {
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
}

// Payload arguments, one per op that takes any.
type (
	ConnectArgs struct {
		Values  map[string]string `json:"values"`
		Account string            `json:"account"`
		PixKey  string            `json:"pix_key,omitempty"`
	}
	ResealArgs struct {
		CertPEM string `json:"cert_pem"`
		KeyPEM  string `json:"key_pem"`
	}
	TokenArgs struct {
		Set Capability `json:"set"`
	}
	StatementArgs struct {
		From Date `json:"from"`
		To   Date `json:"to"`
		Page int  `json:"page"`
	}
	BalanceArgs struct {
		At Date `json:"at"`
	}
	RefArgs struct {
		Ref string `json:"ref"`
	}
	UpdateArgs struct {
		Ref    string `json:"ref"`
		Amount int64  `json:"amount"`
	}
	GetRefundArgs struct {
		PaymentID string `json:"payment_id"`
		RefundID  string `json:"refund_id"`
	}
	WebhookArgs struct {
		Product Product `json:"product"`
		URL     string  `json:"url,omitempty"`
	}
	CallbacksArgs struct {
		Since time.Time `json:"since"`
	}
	RetryArgs struct {
		Refs []string `json:"refs"`
	}
	WebhookResult struct {
		URL string `json:"url"`
	}
)
