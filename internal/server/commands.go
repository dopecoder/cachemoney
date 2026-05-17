package server

import "context"

// engineCtx is the context handlers pass to the engine. It is deliberately not
// cancelled by Shutdown: an in-flight request must complete and deliver its reply
// (the drain bound is enforced by closing the socket, not by aborting the engine).
func engineCtx() context.Context { return context.Background() }

// cmdPing answers PING. Argless PING replies "+PONG\r\n"; "PING <msg>" echoes msg
// as a bulk string.
func cmdPing(c *conn, args [][]byte) error {
	if len(args) == 2 {
		return c.w.WriteBulk(args[1])
	}
	return c.w.WriteSimpleString("PONG")
}

// cmdQuit replies "+OK\r\n" and asks the loop to close the connection.
func cmdQuit(c *conn, _ [][]byte) error {
	_ = c.w.WriteSimpleString("OK")
	return errClientQuit
}

// cmdGet replies with the live value as a bulk string, or the null bulk for a
// missing/expired key.
func cmdGet(c *conn, args [][]byte) error {
	v, ok, err := c.srv.engine.Get(engineCtx(), string(args[1]))
	if err != nil {
		return c.replyEngineErr(err)
	}
	if !ok {
		return c.w.WriteNull()
	}
	return c.w.WriteBulk(v)
}

// cmdSet stores a persistent value (ttl 0) and replies "+OK\r\n". Expiry options
// arrive in PR B.
func cmdSet(c *conn, args [][]byte) error {
	if err := c.srv.engine.Set(engineCtx(), string(args[1]), args[2], 0); err != nil {
		return c.replyEngineErr(err)
	}
	return c.w.WriteSimpleString("OK")
}

// cmdDel removes a single key, replying ":1\r\n" when it was present and live, or
// ":0\r\n" otherwise. Variadic DEL arrives in PR B.
func cmdDel(c *conn, args [][]byte) error {
	existed, err := c.srv.engine.Del(engineCtx(), string(args[1]))
	if err != nil {
		return c.replyEngineErr(err)
	}
	if existed {
		return c.w.WriteInt(1)
	}
	return c.w.WriteInt(0)
}
