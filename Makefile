# Copyright 2011 Google Inc. All Rights Reserved.
# This file is available under the Apache license.

GOFILES=\
	ast.go\
	compiler.go\
	lexer.go\
	main.go\
	metric.go\
	parser.go\
	symtab.go\
	tail.go\
	unparser.go\
	vm.go\
	watch.go\
	collectd.go\
	graphite.go\

GOTESTFILES=\
	ex_test.go\
	lexer_test.go\
	parser_test.go\
	tail_test.go\
	vm_test.go\
	watch_test.go\
	bench_test.go\

CLEANFILES+=\
	parser.go\
	y.output\

all: emtail

emtail: parser.go $(GOFILES)
	go build

parser.go: parser.y
	go tool yacc -v y.output -o $@ -p Emtail $<

.PHONY: test
test: parser.go $(GOFILES) $(GOTESTFILES)
	go test -test.v=true

.PHONY: testshort
testshort: parser.go $(GOFILES) $(GOTESTFILES)
	go test -test.short

.PHONY: bench
bench: parser.go $(GOFILES) $(GOTESTFILES)
	go test -test.bench=.*

.PHONY: testall
testall: test bench
