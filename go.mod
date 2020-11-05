module github.com/ngrok/tableroll

require (
	github.com/euank/filelock v0.0.0-20200318073246-6ea232a62104
	github.com/euank/tableroll v1.0.1-0.20190509205925-1c7855724129
	github.com/inconshreveable/log15 v0.0.0-20180818164646-67afb5ed74ec
	github.com/opencontainers/runc v0.0.0-00010101000000-000000000000
	github.com/pkg/errors v0.8.1
	github.com/rkt/rkt v1.30.0 // indirect
	github.com/stretchr/testify v1.6.1
	golang.org/x/sync v0.0.0-20201020160332-67f06af15bc9
	golang.org/x/sys v0.0.0-20201101102859-da207088b7d1
	k8s.io/utils v0.0.0-20190221042446-c2654d5206da
)

replace github.com/opencontainers/runc => github.com/kevinburke/runc v1.0.0-rc8.0.20190502181430-3ec7f94c7eff

go 1.13
