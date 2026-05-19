package server

import "strconv"

// cmdHello implements RESP's HELLO handshake: protocol negotiation only (no auth,
// single keyspace). The grammar is
// HELLO [protover [AUTH user pass] [SETNAME name]]:
//
//   - bare HELLO          -> reply the server map at the current dialect (no flip).
//   - HELLO 2 / HELLO 3   -> flip the connection dialect via SetProto, then reply.
//   - HELLO 1 / 4 / abc   -> "-NOPROTO unsupported protocol version" (no flip).
//   - …AUTH user pass     -> "-ERR Client sent AUTH, but no password is set" (no
//     auth is introduced; silently honoring it would mislead the client).
//   - …SETNAME name       -> accepted and ignored (no client name is tracked at M0).
//
// The dialect flip is the observable contract: the same logical WriteNull
// serializes as "$-1\r\n" before/after HELLO 2 and as "_\r\n" after HELLO 3.
func cmdHello(c *conn, args [][]byte) error {
	proto := c.proto
	flip := false
	rest := args[1:]
	if len(rest) > 0 {
		v, err := strconv.Atoi(string(rest[0]))
		if err != nil || v < 2 || v > 3 {
			return c.w.WriteError("NOPROTO unsupported protocol version")
		}
		proto = v
		flip = true
		rest = rest[1:]
	}
	for i := 0; i < len(rest); {
		switch asciiUpper(rest[i]) {
		case "AUTH":
			return c.w.WriteError("ERR Client sent AUTH, but no password is set")
		case "SETNAME":
			if i+1 >= len(rest) {
				return c.w.WriteError(errSyntax)
			}
			i += 2 // consume SETNAME + its name (ignored)
		default:
			return c.w.WriteError(errSyntax)
		}
	}
	if flip {
		c.w.SetProto(proto)
		c.proto = proto
	}
	return c.writeHelloMap(proto)
}

// writeHelloMap emits the seven-field server map. WriteMapHeader downgrades to a
// flat "*14" array under RESP2 automatically, so one code path serves both
// dialects. The sticky-error Writer means a mid-map failure surfaces at the final
// call (and at Flush).
func (c *conn) writeHelloMap(proto int) error {
	w := c.w
	_ = w.WriteMapHeader(7)
	_ = w.WriteBulkString("server")
	_ = w.WriteBulkString("cachemoney")
	_ = w.WriteBulkString("version")
	_ = w.WriteBulkString(c.srv.cfg.Version)
	_ = w.WriteBulkString("proto")
	_ = w.WriteInt(int64(proto))
	_ = w.WriteBulkString("id")
	_ = w.WriteInt(int64(c.id)) //nolint:gosec // c.id is a monotonic per-process counter; it never approaches int64 overflow
	_ = w.WriteBulkString("mode")
	_ = w.WriteBulkString("standalone")
	_ = w.WriteBulkString("role")
	_ = w.WriteBulkString("master")
	_ = w.WriteBulkString("modules")
	return w.WriteArrayHeader(0)
}

// cmdCommand answers the stock redis-cli COMMAND probe with the minimum that keeps
// it quiet: bare COMMAND and COMMAND DOCS reply an empty array; COMMAND COUNT
// reports the honest registered-command count; any other sub-command is a
// non-fatal -ERR.
func cmdCommand(c *conn, args [][]byte) error {
	if len(args) == 1 {
		return c.w.WriteArrayHeader(0)
	}
	switch asciiUpper(args[1]) {
	case "DOCS":
		return c.w.WriteArrayHeader(0)
	case "COUNT":
		return c.w.WriteInt(int64(len(commandTable)))
	default:
		return c.w.WriteError("ERR Unknown COMMAND subcommand or wrong number of arguments")
	}
}

// cmdConfig answers CONFIG GET with an empty array (spec: "non-error array,
// possibly empty") and CONFIG SET with a no-op +OK; any other sub-command is a
// non-fatal -ERR.
func cmdConfig(c *conn, args [][]byte) error {
	switch asciiUpper(args[1]) {
	case "GET":
		return c.w.WriteArrayHeader(0)
	case "SET":
		return c.w.WriteSimpleString("OK")
	default:
		return c.w.WriteError("ERR Unknown CONFIG subcommand or wrong number of arguments")
	}
}

// cmdSelect implements the single-keyspace SELECT: index 0 is the no-op success,
// every other parseable index is out of range, and a non-numeric index is the
// not-an-integer error. All replies keep the connection open.
func cmdSelect(c *conn, args [][]byte) error {
	n, err := strconv.Atoi(string(args[1]))
	if err != nil {
		return c.w.WriteError(errNotInteger)
	}
	if n != 0 {
		return c.w.WriteError("ERR DB index is out of range")
	}
	return c.w.WriteSimpleString("OK")
}
