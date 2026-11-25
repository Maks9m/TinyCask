// Package server is the TCP front end: it parses the RESP2 subset and inline
// commands, dispatches them to a store it reaches through its own small
// interface, and writes the replies (ARCHITECTURE §9).
package server
