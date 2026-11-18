package engine

import (
	"context"
	"encoding/binary"
	"net"
)

// OnAccept invokes the ABI v4 L4 on_accept(ip_hi, ip_lo, port) hook. Any
// nonzero result denies; the hook cannot route. If the module does not export
// on_accept, the connection is allowed without touching the instance pool.
func (e *Engine) OnAccept(ctx context.Context, ip net.IP, port int) (Verdict, error) {
	if e == nil || !e.hasAccept {
		return VerdictAllow, nil
	}
	inst, err := e.borrow(ctx)
	if err != nil {
		return VerdictDeny, err
	}
	hi, lo := encodeIP(ip)
	inst.stack[0] = hi
	inst.stack[1] = lo
	inst.stack[2] = uint64(port) & 0xffffffff // #nosec G115 -- TCP port is 0..65535
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	callErr := inst.onAccept.CallWithStack(callCtx, inst.stack[:3])
	cancel()
	if callErr != nil {
		e.discard(inst)
		return VerdictDeny, callErr
	}
	v := int32(uint32(inst.stack[0])) // #nosec G115 -- WASM i32 result
	e.release(inst)
	if v != 0 {
		return VerdictDeny, nil
	}
	return VerdictAllow, nil
}

// OnCloseConn invokes on_close_ex(conn_id, ip_hi, ip_lo, rx, tx) when exported,
// otherwise on_close(ip_hi, ip_lo, rx, tx). It is a best-effort lifecycle hook:
// callers log/discard a failed close but do not resurrect the connection.
func (e *Engine) OnCloseConn(ctx context.Context, connID uint64, ip net.IP, rxBytes, txBytes int64) error {
	if e == nil || (!e.hasClose && !e.hasCloseEx) {
		return nil
	}
	inst, err := e.borrow(ctx)
	if err != nil {
		return err
	}
	hi, lo := encodeIP(ip)
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	var callErr error
	if e.hasCloseEx {
		inst.stack[0] = connID
		inst.stack[1] = hi
		inst.stack[2] = lo
		inst.stack[3] = uint64(rxBytes) // #nosec G115 -- raw i64 bit pattern
		inst.stack[4] = uint64(txBytes) // #nosec G115 -- raw i64 bit pattern
		callErr = inst.onCloseEx.CallWithStack(callCtx, inst.stack[:5])
	} else {
		inst.stack[0] = hi
		inst.stack[1] = lo
		inst.stack[2] = uint64(rxBytes) // #nosec G115 -- raw i64 bit pattern
		inst.stack[3] = uint64(txBytes) // #nosec G115 -- raw i64 bit pattern
		callErr = inst.onClose.CallWithStack(callCtx, inst.stack[:4])
	}
	cancel()
	if callErr != nil {
		e.discard(inst)
		return callErr
	}
	e.release(inst)
	return nil
}

// encodeIP packs an IP into two big-endian uint64 values using the IPv4-mapped
// IPv6 representation (::ffff:a.b.c.d) for IPv4 addresses.
func encodeIP(ip net.IP) (hi, lo uint64) {
	if v4 := ip.To4(); v4 != nil {
		lo = 0x0000ffff00000000 | uint64(binary.BigEndian.Uint32(v4))
		return 0, lo
	}
	if len(ip) == net.IPv6len {
		return binary.BigEndian.Uint64(ip[:8]), binary.BigEndian.Uint64(ip[8:])
	}
	return 0, 0
}
