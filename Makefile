.PHONY: run test sim smoke fmt

run:   ## coordinator + console + simulated participants
	go run ./cmd/coordinator -addr 127.0.0.1:8080

test:
	go test ./... -count=1

sim:   ## 20k orders with chaos, manual drill and reconciliation
	go run ./cmd/simulator -n 20000 -c 64 -chaos 1 -drill=true -timeout 1500 -report /tmp/dtx-report.json

smoke: ## fast run without injected faults
	go run ./cmd/simulator -n 500 -c 32 -chaos 0 -drill=false -timeout 1500

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
