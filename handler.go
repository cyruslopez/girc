// Copyright (c) Liam Stanley <me@liamstanley.io>. All rights reserved. Use
// of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package girc

import (
	"fmt"
	"log"
	"math/rand"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// RunHandlers manually runs handlers for a given event.
func (c *Client) RunHandlers(event *Event) {
	if event == nil {
		return
	}

	// Log the event.
	c.debug.Print("< " + StripRaw(event.String()))
	if c.Config.Out != nil {
		if pretty, ok := event.Pretty(); ok {
			fmt.Fprintln(c.Config.Out, StripRaw(pretty))
		}
	}

	// Regular wildcard handlers.
	c.Handlers.exec(ALLEVENTS, c, event.Copy())

	// Then regular handlers.
	c.Handlers.exec(event.Command, c, event.Copy())

	// Check if it's a CTCP.
	if ctcp := decodeCTCP(event.Copy()); ctcp != nil {
		// Execute it.
		c.CTCP.call(c, ctcp)
	}
}

// Handler is lower level implementation of a handler. See
// Caller.AddHandler()
type Handler interface {
	Execute(*Client, Event)
}

// HandlerFunc is a type that represents the function necessary to
// implement Handler.
type HandlerFunc func(client *Client, event Event)

// Execute calls the HandlerFunc with the sender and irc message.
func (f HandlerFunc) Execute(client *Client, event Event) {
	f(client, event)
}

// Caller manages internal and external (user facing) handlers.
type Caller struct {
	// mu is the mutex that should be used when accessing handlers.
	mu sync.RWMutex
	// wg is the waitgroup which is used to execute all handlers concurrently.
	wg sync.WaitGroup

	// external/internal keys are of structure:
	//   map[COMMAND][CUID]Handler
	// Also of note: "COMMAND" should always be uppercase for normalization.

	// external is a map of user facing handlers.
	external map[string]map[string]Handler
	// internal is a map of internally used handlers for the client.
	internal map[string]map[string]Handler
	// debug is the clients logger used for debugging.
	debug *log.Logger
}

// newCaller creates and initializes a new handler.
func newCaller(debugOut *log.Logger) *Caller {
	c := &Caller{
		external: map[string]map[string]Handler{},
		internal: map[string]map[string]Handler{},
		debug:    debugOut,
	}

	return c
}

// Len returns the total amount of user-entered registered handlers.
func (c *Caller) Len() int {
	var total int

	c.mu.RLock()
	for command := range c.external {
		total += len(c.external[command])
	}
	c.mu.RUnlock()

	return total
}

// Count is much like Caller.Len(), however it counts the number of
// registered handlers for a given command.
func (c *Caller) Count(cmd string) int {
	var total int

	cmd = strings.ToUpper(cmd)

	c.mu.RLock()
	for command := range c.external {
		if command == cmd {
			total += len(c.external[command])
		}
	}
	c.mu.RUnlock()

	return total
}

func (c *Caller) String() string {
	var total int

	c.mu.RLock()
	for cmd := range c.internal {
		total += len(c.internal[cmd])
	}
	c.mu.RUnlock()

	return fmt.Sprintf("<Caller external:%d internal:%d>", c.Len(), total)
}

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// cuid generates a unique UID string for each handler for ease of removal.
func (c *Caller) cuid(cmd string, n int) (cuid, uid string) {
	b := make([]byte, n)

	for i := range b {
		b[i] = letterBytes[rand.Int63()%int64(len(letterBytes))]
	}

	return cmd + ":" + string(b), string(b)
}

// cuidToID allows easy mapping between a generated cuid and the caller
// external/internal handler maps.
func (c *Caller) cuidToID(input string) (cmd, uid string) {
	i := strings.IndexByte(input, 0x3A)
	if i < 0 {
		return "", ""
	}

	return input[:i], input[i+1:]
}

type execStack struct {
	Handler
	cuid string
}

// exec executes all handlers pertaining to specified event. Internal first,
// then external.
//
// Please note that there is no specific order/priority for which the
// handler types themselves or the handlers are executed.
func (c *Caller) exec(command string, client *Client, event *Event) {
	// Build a stack of handlers which can be executed concurrently.
	var stack []execStack

	c.mu.RLock()
	// Get internal handlers first.
	if _, ok := c.internal[command]; ok {
		for cuid := range c.internal[command] {
			stack = append(stack, execStack{c.internal[command][cuid], cuid})
		}
	}

	// Aaand then external handlers.
	if _, ok := c.external[command]; ok {
		for cuid := range c.external[command] {
			stack = append(stack, execStack{c.external[command][cuid], cuid})
		}
	}
	c.mu.RUnlock()

	// Run all handlers concurrently across the same event. This should
	// still help prevent mis-ordered events, while speeding up the
	// execution speed.
	c.wg.Add(len(stack))
	for i := 0; i < len(stack); i++ {
		go func(index int) {
			c.debug.Printf("executing handler %s for event %s (%d of %d)", stack[index].cuid, command, index+1, len(stack))
			start := time.Now()

			// If they want to catch any panics, add to defer stack.
			if client.Config.RecoverFunc != nil {
				defer recoverHandlerPanic(client, event, stack[index].cuid, 3)
			}

			stack[index].Execute(client, *event)

			c.debug.Printf("execution of %s took %s (%d of %d)", stack[index].cuid, time.Since(start), index+1, len(stack))
			c.wg.Done()
		}(i)
	}

	// Wait for all of the handlers to complete. Not doing this may cause
	// new events from becoming ahead of older handlers.
	c.wg.Wait()
}

// ClearAll clears all external handlers currently setup within the client.
// This ignores internal handlers.
func (c *Caller) ClearAll() {
	c.mu.Lock()
	c.external = map[string]map[string]Handler{}
	c.mu.Unlock()

	c.debug.Print("cleared all external handlers")
}

// clearInternal clears all internal handlers currently setup within the
// client.
func (c *Caller) clearInternal() {
	c.mu.Lock()
	c.internal = map[string]map[string]Handler{}
	c.mu.Unlock()

	c.debug.Print("cleared all internal handlers")
}

// Clear clears all of the handlers for the given event.
// This ignores internal handlers.
func (c *Caller) Clear(cmd string) {
	cmd = strings.ToUpper(cmd)

	c.mu.Lock()
	if _, ok := c.external[cmd]; ok {
		delete(c.external, cmd)
	}
	c.mu.Unlock()

	c.debug.Printf("cleared external handlers for %s", cmd)
}

// Remove removes the handler with cuid from the handler stack. success
// indicates that it existed, and has been removed. If not success, it
// wasn't a registered handler.
func (c *Caller) Remove(cuid string) (success bool) {
	c.mu.Lock()
	success = c.remove(cuid)
	c.mu.Unlock()

	return success
}

// remove is much like Remove, however is NOT concurrency safe. Lock Caller.mu
// on your own.
func (c *Caller) remove(cuid string) (success bool) {
	cmd, uid := c.cuidToID(cuid)
	if len(cmd) == 0 || len(uid) == 0 {
		return false
	}

	// Check if the irc command/event has any handlers on it.
	if _, ok := c.external[cmd]; !ok {
		return false
	}

	// Check to see if it's actually a registered handler.
	if _, ok := c.external[cmd][uid]; !ok {
		return false
	}

	delete(c.external[cmd], uid)
	c.debug.Printf("removed handler %s", cuid)

	// Assume success.
	return true
}

// sregister is much like Caller.register(), except that it safely locks
// the Caller mutex.
func (c *Caller) sregister(internal bool, cmd string, handler Handler) (cuid string) {
	c.mu.Lock()
	cuid = c.register(internal, cmd, handler)
	c.mu.Unlock()

	return cuid
}

// register will register a handler in the internal tracker. Unsafe (you
// must lock c.mu yourself!)
func (c *Caller) register(internal bool, cmd string, handler Handler) (cuid string) {
	var uid string

	cmd = strings.ToUpper(cmd)

	if internal {
		if _, ok := c.internal[cmd]; !ok {
			c.internal[cmd] = map[string]Handler{}
		}

		cuid, uid = c.cuid(cmd, 20)
		c.internal[cmd][uid] = handler
	} else {
		if _, ok := c.external[cmd]; !ok {
			c.external[cmd] = map[string]Handler{}
		}

		cuid, uid = c.cuid(cmd, 20)
		c.external[cmd][uid] = handler
	}

	_, file, line, _ := runtime.Caller(3)

	c.debug.Printf("registering handler for %q with cuid %q (internal: %t) from: %s:%d", cmd, cuid, internal, file, line)

	return cuid
}

// AddHandler registers a handler (matching the handler interface) for the
// given event. cuid is the handler uid which can be used to remove the
// handler with Caller.Remove().
func (c *Caller) AddHandler(cmd string, handler Handler) (cuid string) {
	return c.sregister(false, cmd, handler)
}

// Add registers the handler function for the given event. cuid is the
// handler uid which can be used to remove the handler with Caller.Remove().
func (c *Caller) Add(cmd string, handler func(client *Client, event Event)) (cuid string) {
	return c.sregister(false, cmd, HandlerFunc(handler))
}

// AddBg registers the handler function for the given event and executes it
// in a go-routine. cuid is the handler uid which can be used to remove the
// handler with Caller.Remove().
func (c *Caller) AddBg(cmd string, handler func(client *Client, event Event)) (cuid string) {
	return c.sregister(false, cmd, HandlerFunc(func(client *Client, event Event) {
		// Setting up background-based handlers this way allows us to get
		// clean call stacks for use with panic recovery.
		go func() {
			// If they want to catch any panics, add to defer stack.
			if client.Config.RecoverFunc != nil {
				defer recoverHandlerPanic(client, &event, "goroutine", 3)
			}

			handler(client, event)
		}()
	}))
}

// AddTmp adds a "temporary" handler, which is good for one-time or few-time
// uses. This supports a deadline and/or manual removal, as this differs
// much from how normal handlers work. An example of a good use for this
// would be to capture the entire output of a multi-response query to the
// server. (e.g. LIST, WHOIS, etc)
//
// The supplied handler is able to return a boolean, which if true, will
// remove the handler from the handler stack.
//
// Additionally, AddTmp has a useful option, deadline. When set to greater
// than 0, deadline will be the amount of time that passes before the handler
// is removed from the stack, regardless if the handler returns true or not.
// This is useful in that it ensures that the handler is cleaned up if the
// server does not respond appropriately, or takes too long to respond.
//
// Note that handlers supplied with AddTmp are executed in a goroutine to
// ensure that they are not blocking other handlers. Additionally, use cuid
// with Caller.Remove() to prematurely remove the handler from the stack,
// bypassing the timeout or waiting for the handler to return that it wants
// to be removed from the stack.
func (c *Caller) AddTmp(cmd string, deadline time.Duration, handler func(client *Client, event Event) bool) (cuid string, done chan struct{}) {
	var uid string
	cuid, uid = c.cuid(cmd, 20)

	done = make(chan struct{})

	c.mu.Lock()
	if _, ok := c.external[cmd]; !ok {
		c.external[cmd] = map[string]Handler{}
	}
	c.external[cmd][uid] = HandlerFunc(func(client *Client, event Event) {
		// Setting up background-based handlers this way allows us to get
		// clean call stacks for use with panic recovery.
		go func() {
			// If they want to catch any panics, add to defer stack.
			if client.Config.RecoverFunc != nil {
				defer recoverHandlerPanic(client, &event, "tmp-goroutine", 3)
			}

			remove := handler(client, event)
			if remove {
				if ok := c.Remove(cuid); ok {
					close(done)
				}
			}
		}()
	})
	c.mu.Unlock()

	if deadline > 0 {
		go func() {
			<-time.After(deadline)
			if ok := c.Remove(cuid); ok {
				close(done)
			}
		}()
	}

	return cuid, done
}

// recoverHandlerPanic is used to catch all handler panics, and re-route
// them if necessary.
func recoverHandlerPanic(client *Client, event *Event, id string, skip int) {
	perr := recover()
	if perr == nil {
		return
	}

	var file string
	var line int
	var ok bool

	_, file, line, ok = runtime.Caller(skip)

	err := &HandlerError{
		Event:  *event,
		ID:     id,
		File:   file,
		Line:   line,
		Panic:  perr,
		Stack:  debug.Stack(),
		callOk: ok,
	}

	client.Config.RecoverFunc(client, err)
	return
}

// HandlerError is the error returned when a panic is intentionally recovered
// from. It contains useful information like the handler identifier (if
// applicable), filename, line in file where panic occurred, the call
// trace, and original event.
type HandlerError struct {
	Event  Event
	ID     string
	File   string
	Line   int
	Panic  interface{}
	Stack  []byte
	callOk bool
}

// Error returns a prettified version of HandlerError, containing ID, file,
// line, and basic error string.
func (e *HandlerError) Error() string {
	if e.callOk {
		return fmt.Sprintf("panic during handler [%s] execution in %s:%d: %s", e.ID, e.File, e.Line, e.Panic)
	}

	return fmt.Sprintf("panic during handler [%s] execution in unknown: %s", e.ID, e.Panic)
}

// String returns the error that panic returned, as well as the entire call
// trace of where it originated.
func (e *HandlerError) String() string {
	return fmt.Sprintf("panic: %s\n\n%s", e.Panic, string(e.Stack))
}

// DefaultRecoverHandler can be used with Config.RecoverFunc as a default
// catch-all for panics. This will log the error, and the call trace to the
// debug log (see Config.Debug), or os.Stdout if Config.Debug is unset.
func DefaultRecoverHandler(client *Client, err *HandlerError) {
	if client.Config.Debug == nil {
		fmt.Println(err.Error())
		fmt.Println(err.String())
		return
	}

	client.debug.Println(err.Error())
	client.debug.Println(err.String())
}
