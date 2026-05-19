package server

// command is one row of the dispatch table: a canonical name, an inclusive arity
// window over the WHOLE argument vector (including args[0], the verb), and the
// handler. A maxArgs of -1 means unbounded (variadic).
type command struct {
	name    string
	minArgs int
	maxArgs int
	handler func(c *conn, args [][]byte) error
}

// commandTable maps an upper-cased command name to its command. It is populated in
// init (not as a composite-literal initializer) so a handler may legally reference
// the table itself — COMMAND COUNT reports len(commandTable), which would otherwise
// form an initialization cycle. A new command is one row plus one handler.
var commandTable map[string]command

func init() {
	commandTable = map[string]command{
		"PING":    {name: "PING", minArgs: 1, maxArgs: 2, handler: cmdPing},
		"QUIT":    {name: "QUIT", minArgs: 1, maxArgs: 1, handler: cmdQuit},
		"GET":     {name: "GET", minArgs: 2, maxArgs: 2, handler: cmdGet},
		"SET":     {name: "SET", minArgs: 3, maxArgs: 5, handler: cmdSet},
		"DEL":     {name: "DEL", minArgs: 2, maxArgs: -1, handler: cmdDel},
		"EXISTS":  {name: "EXISTS", minArgs: 2, maxArgs: -1, handler: cmdExists},
		"TTL":     {name: "TTL", minArgs: 2, maxArgs: 2, handler: cmdTTL},
		"PTTL":    {name: "PTTL", minArgs: 2, maxArgs: 2, handler: cmdPTTL},
		"HELLO":   {name: "HELLO", minArgs: 1, maxArgs: -1, handler: cmdHello},
		"COMMAND": {name: "COMMAND", minArgs: 1, maxArgs: -1, handler: cmdCommand},
		"CONFIG":  {name: "CONFIG", minArgs: 2, maxArgs: -1, handler: cmdConfig},
		"SELECT":  {name: "SELECT", minArgs: 2, maxArgs: 2, handler: cmdSelect},
	}
}

// dispatch validates the command name and arity centrally, then runs the handler.
// It returns true when the connection should close after the reply is flushed
// (QUIT, or a write/transport error from the handler). Unknown-command and
// wrong-arity errors reply "-ERR ..." and keep the connection open (return false).
func (s *Server) dispatch(c *conn, args [][]byte) bool {
	if len(args) == 0 {
		return false // empty array frame: ignore, keep open
	}
	verb := asciiUpper(args[0])
	cmd, ok := commandTable[verb]
	if !ok {
		c.replyUnknownCommand(args[0])
		return false
	}
	if len(args) < cmd.minArgs || (cmd.maxArgs >= 0 && len(args) > cmd.maxArgs) {
		c.replyWrongArity(cmd.name)
		return false
	}
	// A non-nil handler error is either errClientQuit or a write/transport error;
	// both stop the loop after the pending reply is flushed.
	return cmd.handler(c, args) != nil
}

// asciiUpper upper-cases ASCII letters in b and returns the result as a string.
// Command names are ASCII, so a byte-wise fold is correct and allocation-light.
func asciiUpper(b []byte) string {
	out := make([]byte, len(b))
	for i, ch := range b {
		if ch >= 'a' && ch <= 'z' {
			ch -= 'a' - 'A'
		}
		out[i] = ch
	}
	return string(out)
}
