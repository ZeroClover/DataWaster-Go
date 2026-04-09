# Delegate to build.sh (single source of truth for release flags).
.PHONY: all clean help

all:
	@./build.sh all

clean:
	@./build.sh clean

help:
	@./build.sh help
