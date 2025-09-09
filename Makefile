.PHONY: build
build:
	GOOS=js GOARCH=wasm go build -o mos.deflate.wasm .
