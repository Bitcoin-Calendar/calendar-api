// Package tests holds the service's test suite.
//
// These are black-box tests: they build the real binary with the real build
// tags, start it against a database fixture staged exactly as the deployed
// artifact is (mode 0444, in a 0555 directory), and exercise it over HTTP.
//
// They live here rather than beside the code because Go ties a package to its
// directory: a test in this directory cannot reach package main's unexported
// identifiers. That constraint turned out to suit the subject. Every property
// worth protecting here is a property of the running service — that it boots
// against a read-only artifact, that it never writes to it, that the JSON
// contract the Telegram bot reads is what it expects — and testing the binary
// proves those in a way that calling InitDB directly cannot. In particular the
// build tag itself gets exercised, which is the failure this service is least
// able to notice on its own.
//
// Run them with the tag, via the Makefile:
//
//	make test
package tests
