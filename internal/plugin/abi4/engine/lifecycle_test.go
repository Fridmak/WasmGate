package engine

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestEncodeIP(t *testing.T) {
	hi, lo := encodeIP(net.IPv4(127, 0, 0, 99))
	if hi != 0 || lo != 0x0000ffff7f000063 {
		t.Fatalf("IPv4 encoding = (%#x, %#x)", hi, lo)
	}
	hi, lo = encodeIP(net.ParseIP("2001:db8::1"))
	if hi != 0x20010db800000000 || lo != 1 {
		t.Fatalf("IPv6 encoding = (%#x, %#x)", hi, lo)
	}
}

func TestEngineLifecycle(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, lifecycleModule(0, CapLifecycle), Options{Plan: InspectLifecycle, Timeout: time.Second})
	if err != nil {
		t.Fatalf("New allow: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnAccept(ctx, net.IPv4(127, 0, 0, 1), 1234); err != nil || v != VerdictAllow {
		t.Fatalf("allow lifecycle = (%v, %v)", v, err)
	}
	if err := e.OnCloseConn(ctx, 1, net.IPv4(127, 0, 0, 1), 10, 20); err != nil {
		t.Fatalf("on_close: %v", err)
	}

	deny, err := New(ctx, lifecycleModule(1, CapLifecycle), Options{Plan: InspectLifecycle, Timeout: time.Second})
	if err != nil {
		t.Fatalf("New deny: %v", err)
	}
	defer deny.Close(ctx)
	if v, err := deny.OnAccept(ctx, net.IPv4(127, 0, 0, 99), 1234); err != nil || v != VerdictDeny {
		t.Fatalf("deny lifecycle = (%v, %v)", v, err)
	}
}

func TestEngineLifecycleRejectsEventModule(t *testing.T) {
	if _, err := New(context.Background(), allowModule(), Options{Plan: InspectLifecycle}); err == nil {
		t.Fatal("event module was accepted for a lifecycle-only plan")
	}
}

func TestLifecycleRequiresCapability(t *testing.T) {
	if e, err := New(context.Background(), lifecycleModule(0, 0), Options{Plan: InspectLifecycle}); err == nil {
		_ = e.Close(context.Background())
		t.Fatal("lifecycle exports loaded without CapLifecycle")
	}
}
