
<div align="center"><img src="https://raw.githubusercontent.com/mweagle/Sparta/master/docs_source/static/site/SpartaLogoLarge.png" />
</div>

# Sparta <p align="center">

[![Build Status](https://travis-ci.org/mweagle/Sparta.svg?branch=master)](https://travis-ci.org/mweagle/Sparta)

[![GoDoc](https://godoc.org/github.com/mweagle/Sparta?status.svg)](https://godoc.org/github.com/mweagle/Sparta)

[![Go Report Card](https://goreportcard.com/badge/github.com/mweagle/Sparta)](https://goreportcard.com/report/github.com/mweagle/Sparta)

Visit [gosparta.io](https://gosparta.io) for complete documentation.

## Overview

Sparta takes a set of _golang_ functions and automatically provisions them in
[AWS Lambda](https://aws.amazon.com/lambda/) as a logical unit.

AWS Lambda functions are defined using the standard [AWS Lambda signatures](https://aws.amazon.com/blogs/compute/announcing-go-support-for-aws-lambda/):

* `func()`
* `func() error`
* `func(TIn) error`
* `func() (TOut, error)`
* `func(context.Context) error`
* `func(context.Context, TIn) error`
* `func(context.Context) (TOut, error)`
* `func(context.Context, TIn) (TOut, error)`

 The TIn and TOut parameters represent encoding/json un/marshallable types.

For instance:

```go
// Standard AWS λ function
func helloWorld(ctx context.Context) (string, error) {
  ...
}
```

where

* `ctx` : The request context that includes both the [AWS Context](https://github.com/aws/aws-lambda-go/blob/master/lambdacontext/context.go) as well as Sparta specific [values](https://godoc.org/github.com/mweagle/Sparta#pkg-constants.)


Consumers define a set of lambda functions and provide them to Sparta to create a self-documenting, self-deploying AWS Lambda binary:

```go
  lambdaFn, _ := sparta.NewAWSLambda("Hello World",
    helloWorld,
    sparta.IAMRoleDefinition{})

  var lambdaFunctions []*sparta.LambdaAWSInfo
  lambdaFunctions = append(lambdaFunctions, lambdaFn)

  err := sparta.Main("HelloWorldStack",
    "My Hello World stack",
    lambdaFunctions,
    nil,
    nil)
```

Visit [gosparta.io](https://gosparta.io) for complete documentation.

## Contributing

Sparta contributions are most welcome. Please consult the latest [issues](https://github.com/mweagle/Sparta/issues) for open issues.

### Building

Locally building or testing Sparta itself is typically only needed to make package
changes. Sparta is more often used as a required import of another application.
Building is done via [mage](https://magefile.org/).

To get started building and verifying local changes:

  1. `go get -u -d github.com/magefile/mage`
  1. In the .../mweagle/Sparta directory, run `mage -l` to list the current targets:

  $ mage -l
  Targets:
    build                           the application
    clean                           the working directory
    compareAgainstMasterBranch      is a convenience function to show the comparisons of the current pushed branch against the master branch
    describe                        runs the `TestDescribe` test to generate a describe HTML output file at graph.html
    docsBuild                       builds the public documentation site in the /docs folder
    docsCommit                      builds and commits the current documentation with an autogenerated comment
    docsEdit                        starts a Hugo server and hot reloads the documentation at http://localhost:1313
    docsInstallRequirements         installs the required Hugo version
    ensureAllPreconditions          ensures that the source passes *ALL* static `ensure*` precondition steps
    ensureCleanTree                 ensures that the git tree is clean
    ensureFormatted                 ensures that the source code is formatted with goimports
    ensureGoFmt                     ensures that the source is `gofmt -s` is empty
    ensureLint                      ensures that the source is `golint`ed
    ensureMarkdownSpelling          ensures that all *.MD files are checked for common spelling mistakes
    ensurePrealloc                  ensures that slices that could be preallocated are enforced
    ensureSpelling                  ensures that there are no misspellings in the source
    ensureStaticChecks              ensures that the source code passes static code checks
    ensureTravisBuildEnvironment    is the command that sets up the Travis environment to run the build.
    ensureVet                       ensures that the source has been `go vet`ted
    generateBuildInfo               creates the automatic buildinfo.go file so that we can stamp the SHA into the binaries we build...
    generateConstants               runs the set of commands that update the embedded CONSTANTS for both local and AWS Lambda execution
    installBuildRequirements        installs or updates the dependent packages that aren't referenced by the source, but are needed to build the Sparta source
    logCodeMetrics                  ensures that the source code is formatted with goimports
    publish                         the latest source
    test                            runs the Sparta tests
    testCover                       runs the test and opens up the resulting report
    travisBuild                     is the task to build in the context of a Travis CI pipeline

Confirm tests are passing on `HEAD` by first running `mage -v test`.

As you periodically make local changes, run `mage -v test` to confirm backward compatibility.

### Tests

When possible, please include a [test case](https://golang.org/pkg/testing/) that verifies the local change and ensures compatibility.

## Contributors

Thanks to all Sparta contributors (alphabetical):

* **Kyle Anderson**
* [James Brook](https://github.com/jbrook)
* [Ryan Brown](https://github.com/ryansb)
* [sdbeard](https://github.com/sdbeard)
* [Scott Raine](https://github.com/nylar)
* [Paul Seiffert](https://github.com/seiffert)
* [Thom Shutt](https://github.com/thomshutt)

