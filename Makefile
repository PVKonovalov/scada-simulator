SYSTEM=$(shell uname -s)
PROTOC=protoc
PROTOC_INSTALLED = $(shell which protoc)
PROTOC_INSTALL = brew install protobuf
PYTHON_DST_RIR = ./python
ifeq ("$(SYSTEM)", "Linux")
	PROTOC_INSTALL = sudo apt install -y protobuf-compiler
else ifeq ("$(findstring MINGW, $(SYSTEM))", "MINGW") # Windows
	PROTOC = protoc -I "C:\ProgramData\chocolatey\lib\protoc\tools\include;./"
endif

ifneq ("$(PROTOC_INSTALLED)", "")
	PROTOC_INSTALL=echo protoc installed
endif

export GOBIN=$(shell go env GOPATH)/bin

all: grpc.go

install_protoc:
	$(PROTOC_INSTALL)

install_go_modules:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5

install_py_modules:
	pip install -r requirements.txt

install: install_protoc install_go_modules install_py_modules


grpc.go: api/scada/telemetry.proto api/scada/faultsim.proto api/rtdb/rdss_dms_rtdb_api.proto
# Used ./ output path because of go_package option in .proto files
	$(PROTOC) --go_out=./pkg/ --go-grpc_out=./pkg/ api/scada/telemetry.proto api/scada/faultsim.proto api/rtdb/rdss_dms_rtdb_api.proto


.PHONY: go.mod grpc.go
