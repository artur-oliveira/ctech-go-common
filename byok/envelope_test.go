package byok

import "testing"

func TestEveryOpHasExactlyOneGroup(t *testing.T) {
	want := map[Op]OpGroup{
		OpConnect: GroupRead, OpReseal: GroupRead, OpToken: GroupRead, OpBalance: GroupRead,
		OpStatementPage: GroupRead, OpGetCharge: GroupRead, OpGetPayment: GroupRead, OpGetRefund: GroupRead,
		OpGetWebhook: GroupRead, OpListCallbacks: GroupRead, OpRetryCallbacks: GroupRead,
		OpCancelCharge: GroupTeardown, OpDeleteWebhook: GroupTeardown,
		OpCreateDueCharge: GroupCollect, OpUpdateCharge: GroupCollect, OpRefund: GroupCollect, OpRegisterWebhook: GroupCollect,
		OpSimulatePayment: GroupSandbox,
		OpGetPayout:       GroupRead, OpPayout: GroupPayout, // money out is a class of its own
	}
	if len(want) != len(AllOps) {
		t.Fatalf("AllOps has %d ops, the table %d", len(AllOps), len(want))
	}
	for _, op := range AllOps {
		if got := GroupOf(op); got != want[op] {
			t.Fatalf("GroupOf(%s) = %s, want %s", op, got, want[op])
		}
	}
	if GroupOf("Nope") != "" {
		t.Fatal("an unknown op has no group")
	}
}
