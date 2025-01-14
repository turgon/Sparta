 #!/bin/bash -ex

# Workaround for https://github.com/golang/go/issues/30515
mkdir -pv ./.sparta
cd ./.sparta   
GO111MODULE=off go get -u -v github.com/magefile/mage
GO111MODULE=off go get -u -v github.com/hhatto/gocloc
GO111MODULE=off go get -u -v github.com/mholt/archiver
GO111MODULE=off go get -u -v github.com/pkg/browser
GO111MODULE=off go get -u -v github.com/otiai10/copy
GO111MODULE=off go get -u -v github.com/pkg/errors
GO111MODULE=off go get -u -v honnef.co/go/tools/cmd/...
