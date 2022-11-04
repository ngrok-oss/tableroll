module github.com/ngrok-oss/tableroll/v2

require (
	github.com/euank/filelock v0.0.0-20200318073246-6ea232a62104
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/inconshreveable/log15 v0.0.0-20180818164646-67afb5ed74ec
	github.com/mattn/go-colorable v0.0.9 // indirect
	github.com/mattn/go-isatty v0.0.4 // indirect
	github.com/pkg/errors v0.8.1
	github.com/stretchr/testify v1.6.1
	golang.org/x/sys v0.0.0-20201101102859-da207088b7d1
	k8s.io/utils v0.0.0-20190221042446-c2654d5206da
)

replace github.com/opencontainers/runc => github.com/kevinburke/runc v1.0.0-rc8.0.20190502181430-3ec7f94c7eff

go 1.13
