DUVET ?= duvet

.PHONY: help duvet duvet-ci duvet-open coverage-gate

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | column -t -s ':'

## duvet: extract requirements, build the HTML/JSON report and refresh the snapshot
# --ci false is explicit: duvet turns the snapshot check on by itself when CI
# is set in the environment, which would make this target reject every PR
# that adds a citation. The snapshot is checked only by duvet-ci, at
# milestones.
duvet:
	rm -rf .duvet/requirements
	$(DUVET) report --ci false

## duvet-ci: same as duvet, but fail if .duvet/snapshot.txt would change (milestones only)
duvet-ci:
	rm -rf .duvet/requirements
	$(DUVET) report --ci true

## duvet-open: build the report and open it in a browser
duvet-open: duvet
	xdg-open .duvet/reports/report.html 2>/dev/null || open .duvet/reports/report.html

## coverage-gate: fail unless every ID in IDS has an implementation and a test citation
coverage-gate:
	@if [ -z "$(IDS)" ]; then echo 'usage: make coverage-gate IDS="CL-001 CL-002"' >&2; exit 2; fi
	DUVET=$(DUVET) hack/duvet-coverage.sh $(IDS)
