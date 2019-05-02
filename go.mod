module github.com/ngrok/tableroll

require (
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/hashicorp/errwrap v1.0.0 // indirect
	github.com/inconshreveable/log15 v0.0.0-20180818164646-67afb5ed74ec
	github.com/mattn/go-colorable v0.0.9 // indirect
	github.com/mattn/go-isatty v0.0.4 // indirect
	github.com/opencontainers/runc v0.0.0-00010101000000-000000000000
	github.com/pkg/errors v0.8.1
	github.com/rkt/rkt v1.30.0
	golang.org/x/sys v0.0.0-20190124100055-b90733256f2e
	k8s.io/utils v0.0.0-20190221042446-c2654d5206da
)

replace github.com/opencontainers/runc => github.com/kevinburke/runc v1.0.0-rc8.0.20190502155026-3ec7f94c7effb7b2ca325eeb4a775646d44238a7

go 1.13
