package nut_test

import (
	"context"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/nut"
	"github.com/danotchuuy/home-lab-manager/internal/nut/nuttest"
)

func TestFetch(t *testing.T) {
	srv := nuttest.New(t)
	srv.Set("rack", map[string]string{
		"ups.status":         "OB LB",
		"battery.charge":     "18",
		"battery.runtime":    "240",
		"ups.load":           "35",
		"input.voltage":      "0.0",
		"ups.mfr":            "CyberPower",
		"ups.model":          `CP1500 "PFC"`,
		"device.description": "Rack UPS",
	})
	srv.Set("desk", map[string]string{"ups.status": "OL CHRG"})

	st, err := nut.Fetch(context.Background(), nut.Query{Address: srv.Addr, UPS: "rack"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || !st.OnBattery || !st.LowBattery || st.OnLine || st.Charging {
		t.Errorf("flags = %+v", st)
	}
	if st.Charge == nil || *st.Charge != 18 || st.Runtime == nil || *st.Runtime != 240 || st.Load == nil || *st.Load != 35 {
		t.Errorf("numbers = %+v", st)
	}
	if st.Model != `CyberPower CP1500 "PFC"` {
		t.Errorf("model = %q", st.Model)
	}

	st, err = nut.Fetch(context.Background(), nut.Query{Address: srv.Addr, UPS: "desk"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.OnLine || !st.Charging || st.OnBattery || st.Charge != nil {
		t.Errorf("desk = %+v", st)
	}

	if _, err := nut.Fetch(context.Background(), nut.Query{Address: srv.Addr, UPS: "nope"}); err == nil || !strings.Contains(err.Error(), `no UPS called "nope"`) {
		t.Errorf("unknown UPS: %v", err)
	}
	if _, err := nut.Fetch(context.Background(), nut.Query{Address: srv.Addr, UPS: "a b"}); err == nil {
		t.Error("a name with a space was sent")
	}

	list, err := nut.List(context.Background(), nut.Query{Address: srv.Addr})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "desk" || list[1].Name != "rack" || list[1].Description != "Rack UPS" {
		t.Errorf("list = %+v", list)
	}
}

func TestFetchSignsIn(t *testing.T) {
	srv := nuttest.New(t)
	srv.Username, srv.Password = "monuser", `pa"ss`
	srv.Set("rack", map[string]string{"ups.status": "OL"})
	if _, err := nut.Fetch(context.Background(), nut.Query{Address: srv.Addr, UPS: "rack"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("without signing in: %v", err)
	}
	// The fake server only strips the outer quotes, so check a plain one.
	srv.Password = "secret"
	st, err := nut.Fetch(context.Background(), nut.Query{Address: srv.Addr, UPS: "rack", Username: "monuser", Password: "secret"})
	if err != nil || !st.OnLine {
		t.Errorf("signed in: %+v, %v", st, err)
	}
}

func TestAddress(t *testing.T) {
	for in, want := range map[string]string{
		"nas.lan":        "nas.lan:3493",
		"10.0.0.5:3494":  "10.0.0.5:3494",
		"fd00::5":        "[fd00::5]:3493",
		"[fd00::5]":      "[fd00::5]:3493",
		"[fd00::5]:3493": "[fd00::5]:3493",
		" nas.lan ":      "nas.lan:3493",
	} {
		got, err := nut.Address(in)
		if err != nil || got != want {
			t.Errorf("Address(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "nas lan", "http://nas"} {
		if _, err := nut.Address(bad); err == nil {
			t.Errorf("Address(%q) worked", bad)
		}
	}
}
