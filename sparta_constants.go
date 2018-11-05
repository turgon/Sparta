package sparta

import (
	"strings"

	_ "github.com/aws/aws-lambda-go/lambda"        // Force dep to resolve
	_ "github.com/aws/aws-lambda-go/lambdacontext" // Force dep to resolve
)

////////////////////////////////////////////////////////////////////////////////
// Constants
////////////////////////////////////////////////////////////////////////////////

const (
	// SpartaVersion defines the current Sparta release
	SpartaVersion = "1.6.0"
	// GoLambdaVersion is the Go version runtime used for the lambda function
	GoLambdaVersion = "go1.x"
	// SpartaBinaryName is binary name that exposes the Go lambda function
	SpartaBinaryName = "Sparta.lambda.amd64"
)
const (
	// Custom Resource typename used to create new cloudFormationUserDefinedFunctionCustomResource
	cloudFormationLambda = "Custom::SpartaLambdaCustomResource"
	// divider length is the length of a divider in the text
	// based CLI output
	dividerLength = 48
)
const (
	// envVarLogLevel is the provision time debug value
	// carried into the execution environment
	envVarLogLevel = "SPARTA_LOG_LEVEL"
	// spartaEnvVarFunctionName is the name of this function in the
	// map. It's the function that will be registered to run
	// envVarFunctionName = "SPARTA_FUNC_NAME"
	// envVarDiscoveryInformation is the name of the discovery information
	// published into the environment
	envVarDiscoveryInformation = "SPARTA_DISCOVERY_INFO"
)

var (
	// internal logging header
	headerDivider = strings.Repeat("═", dividerLength)
)

// AWS Principal ARNs from http://docs.aws.amazon.com/general/latest/gr/aws-arns-and-namespaces.html
// See also
// http://docs.aws.amazon.com/general/latest/gr/rande.html
// for region specific principal names
const (
	// @enum AWSPrincipal
	APIGatewayPrincipal = "apigateway.amazonaws.com"
	// @enum AWSPrincipal
	CloudWatchEventsPrincipal = "events.amazonaws.com"
	// @enum AWSPrincipal
	SESPrincipal = "ses.amazonaws.com"
	// @enum AWSPrincipal
	SNSPrincipal = "sns.amazonaws.com"
	// @enum AWSPrincipal
	EC2Principal = "ec2.amazonaws.com"
	// @enum AWSPrincipal
	LambdaPrincipal = "lambda.amazonaws.com"
)

type contextKey int

const (
	// ContextKeyLogger is the request-independent *logrus.Logger
	// instance common to all requests
	ContextKeyLogger contextKey = iota
	// ContextKeyRequestLogger is the *logrus.Entry instance
	// that is annotated with request-identifying
	// information extracted from the AWS context object
	ContextKeyRequestLogger
	// ContextKeyLambdaContext is the *sparta.LambdaContext
	// pointer in the request
	// DEPRECATED
	ContextKeyLambdaContext
)

const (
	// ContextKeyLambdaVersions is the key in the context that stores the map
	// of autoincrementing versions
	ContextKeyLambdaVersions = "spartaLambdaVersions"
)
