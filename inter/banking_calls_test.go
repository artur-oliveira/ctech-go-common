package inter

import (
	"context"
	"testing"

	"gopkg.aoctech.app/api-commons/byok"
)

func TestSaldoReadsTheDay(t *testing.T) {
	c, cr, calls := recorder(t, `{"disponivel":1234.56,"bloqueadoCheque":0,"bloqueadoJudicialmente":10,"bloqueadoAdministrativo":0}`, 200)
	at, _ := byok.ParseDate("2026-06-30")
	s, err := c.Saldo(context.Background(), cr, "b", at)
	if err != nil || s.Disponivel != 1234.56 || (*calls)[0].query != "dataSaldo=2026-06-30" {
		t.Fatalf("%+v %v %+v", s, err, *calls)
	}
}
