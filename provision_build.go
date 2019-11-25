// +build !lambdabinary

package sparta

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/lambda"
	humanize "github.com/dustin/go-humanize"
	spartaAWS "github.com/mweagle/Sparta/aws"
	spartaCF "github.com/mweagle/Sparta/aws/cloudformation"
	spartaS3 "github.com/mweagle/Sparta/aws/s3"
	"github.com/mweagle/Sparta/system"
	spartaZip "github.com/mweagle/Sparta/zip"
	gocc "github.com/mweagle/go-cloudcondenser"
	gocf "github.com/mweagle/go-cloudformation"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

////////////////////////////////////////////////////////////////////////////////
// CONSTANTS
////////////////////////////////////////////////////////////////////////////////
func spartaTagName(baseKey string) string {
	return fmt.Sprintf("io:gosparta:%s", baseKey)
}

var (
	// SpartaTagBuildIDKey is the keyname used in the CloudFormation Output
	// that stores the user-supplied or automatically generated BuildID
	// for this run
	SpartaTagBuildIDKey = spartaTagName("buildId")

	// SpartaTagBuildTagsKey is the keyname used in the CloudFormation Output
	// that stores the optional user-supplied golang build tags
	SpartaTagBuildTagsKey = spartaTagName("buildTags")
)

// finalizerFunction is the type of function pushed onto the cleanup stack
type finalizerFunction func(logger *logrus.Logger)

////////////////////////////////////////////////////////////////////////////////
// Type that encapsulates an S3 URL with accessors to return either the
// full URL or just the valid S3 Keyname
type s3UploadURL struct {
	location string
	path     string
	version  string
}

func (s3URL *s3UploadURL) keyName() string {
	return s3URL.path
}

func newS3UploadURL(s3URL string) *s3UploadURL {
	urlParts, urlPartsErr := url.Parse(s3URL)
	if nil != urlPartsErr {
		return nil
	}
	queryParams, queryParamsErr := url.ParseQuery(urlParts.RawQuery)
	if nil != queryParamsErr {
		return nil
	}
	versionIDValues := queryParams["versionId"]
	version := ""
	if len(versionIDValues) == 1 {
		version = versionIDValues[0]
	}
	return &s3UploadURL{location: s3URL,
		path:    strings.TrimPrefix(urlParts.Path, "/"),
		version: version}
}

////////////////////////////////////////////////////////////////////////////////

func codeZipKey(url *s3UploadURL) string {
	if url == nil {
		return ""
	}
	return url.keyName()
}
func codeZipVersion(url *s3UploadURL) string {
	if url == nil {
		return ""
	}
	return url.version
}

////////////////////////////////////////////////////////////////////////////////
// Represents data associated with provisioning the S3 Site iff defined
type s3SiteContext struct {
	s3Site      *S3Site
	s3UploadURL *s3UploadURL
}

// Type of a workflow step.  Each step is responsible
// for returning the next step or an error if the overall
// workflow should stop.
type workflowStep func(ctx *workflowContext) (workflowStep, error)

// workflowStepDuration represents a discrete step in the provisioning
// workflow.
type workflowStepDuration struct {
	name     string
	duration time.Duration
}

// userdata is user-supplied, code related values
type userdata struct {
	// Is this is a -dry-run?
	noop bool
	// Is this a CGO enabled build?
	useCGO bool
	// Are in-place updates enabled?
	inPlace bool
	// The user-supplied or automatically generated BuildID
	buildID string
	// Optional user-supplied build tags
	buildTags string
	// Optional link flags
	linkFlags string
	// Canonical basename of the service.  Also used as the CloudFormation
	// stack name
	serviceName string
	// Service description
	serviceDescription string
	// The slice of Lambda functions that constitute the service
	lambdaAWSInfos []*LambdaAWSInfo
	// User supplied workflow hooks
	workflowHooks *WorkflowHooks
	// Code pipeline S3 trigger keyname
	codePipelineTrigger string
	// Optional APIGateway definition to associate with this service
	api APIGateway
	// Optional S3 site data to provision together with this service
	s3SiteContext *s3SiteContext
	// The user-supplied S3 bucket where service artifacts should be posted.
	s3Bucket string
}

// context is data that is mutated during the provisioning workflow
type provisionContext struct {
	// Information about the ZIP archive that contains the LambdaCode source
	s3CodeZipURL *s3UploadURL
	// AWS Session to be used for all API calls made in the process of provisioning
	// this service.
	awsSession *session.Session
	// Cached IAM role name map.  Used to support dynamic and static IAM role
	// names.  Static ARN role names are checked for existence via AWS APIs
	// prior to CloudFormation provisioning.
	lambdaIAMRoleNameMap map[string]*gocf.StringExpr
	// IO writer for autogenerated template results
	templateWriter io.Writer
	// CloudFormation Template
	cfTemplate *gocf.Template
	// Is versioning enabled for s3 Bucket?
	s3BucketVersioningEnabled bool
	// name of the binary inside the ZIP archive
	binaryName string
	// Context to pass between workflow operations
	workflowHooksContext map[string]interface{}
}

// similar to context, transaction scopes values that span the entire
// provisioning step
type transaction struct {
	startTime time.Time
	// Optional rollback functions that workflow steps may append to if they
	// have made mutations during provisioning.
	rollbackFunctions []spartaS3.RollbackFunction
	// Optional finalizer functions that are unconditionally executed following
	// workflow completion, success or failure
	finalizerFunctions []finalizerFunction
	// Timings that measure how long things actually took
	stepDurations []*workflowStepDuration
}

////////////////////////////////////////////////////////////////////////////////
// Workflow context
// The workflow context is created by `provision` and provided to all
// functions that constitute the provisioning workflow.
type workflowContext struct {
	// User supplied data that's Lambda specific
	userdata userdata
	// Context that's mutated across the workflow steps
	context provisionContext
	// Transaction-scoped information thats mutated across the workflow
	// steps
	transaction transaction
	// Preconfigured logger
	logger *logrus.Logger
}

// recordDuration is a utility function to record how long
func recordDuration(start time.Time, name string, ctx *workflowContext) {
	elapsed := time.Since(start)
	ctx.transaction.stepDurations = append(ctx.transaction.stepDurations,
		&workflowStepDuration{
			name:     name,
			duration: elapsed,
		})
}

// Register a rollback function in the event that the provisioning
// function failed.
func (ctx *workflowContext) registerRollback(userFunction spartaS3.RollbackFunction) {
	if nil == ctx.transaction.rollbackFunctions || len(ctx.transaction.rollbackFunctions) <= 0 {
		ctx.transaction.rollbackFunctions = make([]spartaS3.RollbackFunction, 0)
	}
	ctx.transaction.rollbackFunctions = append(ctx.transaction.rollbackFunctions, userFunction)
}

// Register a rollback function in the event that the provisioning
// function failed.
func (ctx *workflowContext) registerFinalizer(userFunction finalizerFunction) {
	if nil == ctx.transaction.finalizerFunctions || len(ctx.transaction.finalizerFunctions) <= 0 {
		ctx.transaction.finalizerFunctions = make([]finalizerFunction, 0)
	}
	ctx.transaction.finalizerFunctions = append(ctx.transaction.finalizerFunctions, userFunction)
}

// Register a finalizer that cleans up local artifacts
func (ctx *workflowContext) registerFileCleanupFinalizer(localPath string) {
	cleanup := func(logger *logrus.Logger) {
		errRemove := os.Remove(localPath)
		if nil != errRemove {
			logger.WithFields(logrus.Fields{
				"Path":  localPath,
				"Error": errRemove,
			}).Warn("Failed to cleanup intermediate artifact")
		} else {
			logger.WithFields(logrus.Fields{
				"Path": relativePath(localPath),
			}).Debug("Build artifact deleted")
		}
	}
	ctx.registerFinalizer(cleanup)
}

// Run any provided rollback functions
func (ctx *workflowContext) rollback() {
	defer recordDuration(time.Now(), "Rollback", ctx)

	// Run each cleanup function concurrently.  If there's an error
	// all we're going to do is log it as a warning, since at this
	// point there's nothing to do...
	ctx.logger.Info("Invoking rollback functions")
	var wg sync.WaitGroup
	wg.Add(len(ctx.transaction.rollbackFunctions))
	rollbackErr := callRollbackHook(ctx, &wg)
	if rollbackErr != nil {
		ctx.logger.WithFields(logrus.Fields{
			"Error": rollbackErr,
		}).Warning("Rollback Hook failed to execute")
	}
	for _, eachCleanup := range ctx.transaction.rollbackFunctions {
		go func(cleanupFunc spartaS3.RollbackFunction, goLogger *logrus.Logger) {
			// Decrement the counter when the goroutine completes.
			defer wg.Done()
			// Fetch the URL.
			err := cleanupFunc(goLogger)
			if nil != err {
				ctx.logger.WithFields(logrus.Fields{
					"Error": err,
				}).Warning("Failed to cleanup resource")
			}
		}(eachCleanup, ctx.logger)
	}
	wg.Wait()
}

////////////////////////////////////////////////////////////////////////////////
// Private - START
//

// logFilesize outputs a friendly filesize for the given filepath
func logFilesize(message string, filePath string, logger *logrus.Logger) {
	// Binary size
	stat, err := os.Stat(filePath)
	if err == nil {
		logger.WithFields(logrus.Fields{
			"Size": humanize.Bytes(uint64(stat.Size())),
		}).Info(message)
	}
}

// Encapsulate calling the rollback hooks
func callRollbackHook(ctx *workflowContext, wg *sync.WaitGroup) error {
	if ctx.userdata.workflowHooks == nil {
		return nil
	}
	rollbackHooks := ctx.userdata.workflowHooks.Rollbacks
	if ctx.userdata.workflowHooks.Rollback != nil {
		ctx.logger.Warn("DEPRECATED: Single RollbackHook superseded by RollbackHookHandler slice")
		rollbackHooks = append(rollbackHooks,
			RollbackHookFunc(ctx.userdata.workflowHooks.Rollback))
	}
	for _, eachRollbackHook := range rollbackHooks {
		wg.Add(1)
		go func(handler RollbackHookHandler, context map[string]interface{},
			serviceName string,
			awsSession *session.Session,
			noop bool,
			logger *logrus.Logger) {
			// Decrement the counter when the goroutine completes.
			defer wg.Done()
			rollbackErr := handler.Rollback(context,
				serviceName,
				awsSession,
				noop,
				logger)
			logger.WithFields(logrus.Fields{
				"Error": rollbackErr,
			}).Warn("Rollback function failed to complete")
		}(eachRollbackHook,
			ctx.context.workflowHooksContext,
			ctx.userdata.serviceName,
			ctx.context.awsSession,
			ctx.userdata.noop,
			ctx.logger)
	}
	return nil
}

// Encapsulate calling the service decorator hooks
func callServiceDecoratorHook(ctx *workflowContext) error {
	if ctx.userdata.workflowHooks == nil {
		return nil
	}
	serviceHooks := ctx.userdata.workflowHooks.ServiceDecorators
	if ctx.userdata.workflowHooks.ServiceDecorator != nil {
		ctx.logger.Warn("DEPRECATED: Single ServiceDecorator hook superseded by ServiceDecorators slice")
		serviceHooks = append(serviceHooks,
			ServiceDecoratorHookFunc(ctx.userdata.workflowHooks.ServiceDecorator))
	}
	// If there's an API gateway definition, include the resources that provision it.
	// Since this export will likely
	// generate outputs that the s3 site needs, we'll use a temporary outputs accumulator,
	// pass that to the S3Site
	// if it's defined, and then merge it with the normal output map.-
	for _, eachServiceHook := range serviceHooks {
		hookName := runtime.FuncForPC(reflect.ValueOf(eachServiceHook).Pointer()).Name()
		ctx.logger.WithFields(logrus.Fields{
			"ServiceDecoratorHook": hookName,
			"WorkflowHookContext":  ctx.context.workflowHooksContext,
		}).Info("Calling WorkflowHook")

		serviceTemplate := gocf.NewTemplate()
		decoratorError := eachServiceHook.DecorateService(ctx.context.workflowHooksContext,
			ctx.userdata.serviceName,
			serviceTemplate,
			ctx.userdata.s3Bucket,
			codeZipKey(ctx.context.s3CodeZipURL),
			ctx.userdata.buildID,
			ctx.context.awsSession,
			ctx.userdata.noop,
			ctx.logger)
		if nil != decoratorError {
			return decoratorError
		}
		safeMergeErrs := gocc.SafeMerge(serviceTemplate, ctx.context.cfTemplate)
		if len(safeMergeErrs) != 0 {
			return errors.Errorf("Failed to merge templates: %#v", safeMergeErrs)
		}
	}
	return nil
}

// Encapsulate calling the archive hooks
func callArchiveHook(lambdaArchive *zip.Writer,
	ctx *workflowContext) error {

	if ctx.userdata.workflowHooks == nil {
		return nil
	}
	archiveHooks := ctx.userdata.workflowHooks.Archives
	if ctx.userdata.workflowHooks.Archive != nil {
		ctx.logger.Warn("DEPRECATED: Single ArchiveHook hook superseded by ArchiveHooks slice")
		archiveHooks = append(archiveHooks,
			ArchiveHookFunc(ctx.userdata.workflowHooks.Archive))
	}
	for _, eachArchiveHook := range archiveHooks {
		// Run the hook
		ctx.logger.WithFields(logrus.Fields{
			"WorkflowHookContext": ctx.context.workflowHooksContext,
		}).Info("Calling ArchiveHook")
		hookErr := eachArchiveHook.DecorateArchive(ctx.context.workflowHooksContext,
			ctx.userdata.serviceName,
			lambdaArchive,
			ctx.context.awsSession,
			ctx.userdata.noop,
			ctx.logger)
		if hookErr != nil {
			return errors.Wrapf(hookErr, "DecorateArchive returned an error")
		}
	}
	return nil
}

// Encapsulate calling a workflow hook
func callWorkflowHook(hookPhase string,
	hook WorkflowHook,
	hooks []WorkflowHookHandler,
	ctx *workflowContext) error {

	if hook != nil {
		ctx.logger.Warn(fmt.Sprintf("DEPRECATED: Single %s hook superseded by %ss slice",
			hookPhase,
			hookPhase))
		hooks = append(hooks, WorkflowHookFunc(hook))
	}
	for _, eachHook := range hooks {
		// Run the hook
		ctx.logger.WithFields(logrus.Fields{
			"Phase":               hookPhase,
			"WorkflowHookContext": ctx.context.workflowHooksContext,
		}).Info("Calling WorkflowHook")

		hookErr := eachHook.DecorateWorkflow(ctx.context.workflowHooksContext,
			ctx.userdata.serviceName,
			ctx.userdata.s3Bucket,
			ctx.userdata.buildID,
			ctx.context.awsSession,
			ctx.userdata.noop,
			ctx.logger)
		if hookErr != nil {
			return errors.Wrapf(hookErr, "DecorateWorkflow returned an error")
		}
	}
	return nil
}

// Encapsulate calling the validation hooks
func callValidationHooks(validationHooks []ServiceValidationHookHandler,
	template *gocf.Template,
	ctx *workflowContext) error {

	var marshaledTemplate []byte
	if len(validationHooks) != 0 {
		jsonBytes, jsonBytesErr := json.Marshal(template)
		if jsonBytesErr != nil {
			return errors.Wrapf(jsonBytesErr, "Failed to marshal template for validation")
		}
		marshaledTemplate = jsonBytes
	}

	for _, eachHook := range validationHooks {
		// Run the hook
		ctx.logger.WithFields(logrus.Fields{
			"Phase":                 "Validation",
			"ValidationHookContext": ctx.context.workflowHooksContext,
		}).Info("Calling WorkflowHook")

		var loopTemplate gocf.Template
		unmarshalErr := json.Unmarshal(marshaledTemplate, &loopTemplate)
		if unmarshalErr != nil {
			return errors.Wrapf(unmarshalErr,
				"Failed to unmarshal read-only copy of template for Validation")
		}

		hookErr := eachHook.ValidateService(ctx.context.workflowHooksContext,
			ctx.userdata.serviceName,
			&loopTemplate,
			ctx.userdata.s3Bucket,
			codeZipKey(ctx.context.s3CodeZipURL),
			ctx.userdata.buildID,
			ctx.context.awsSession,
			ctx.userdata.noop,
			ctx.logger)
		if hookErr != nil {
			return errors.Wrapf(hookErr, "Service failed to pass validation")
		}
	}
	return nil
}

// versionAwareS3KeyName returns a keyname that provides the correct cache
// invalidation semantics based on whether the target bucket
// has versioning enabled
func versionAwareS3KeyName(s3DefaultKey string, s3VersioningEnabled bool, logger *logrus.Logger) (string, error) {
	versionKeyName := s3DefaultKey
	if !s3VersioningEnabled {
		var extension = path.Ext(s3DefaultKey)
		var prefixString = strings.TrimSuffix(s3DefaultKey, extension)

		hash := sha1.New()
		salt := fmt.Sprintf("%s-%d", s3DefaultKey, time.Now().UnixNano())
		_, writeErr := hash.Write([]byte(salt))
		if writeErr != nil {
			return "", errors.Wrapf(writeErr, "Failed to update hash digest")
		}
		versionKeyName = fmt.Sprintf("%s-%s%s",
			prefixString,
			hex.EncodeToString(hash.Sum(nil)),
			extension)

		logger.WithFields(logrus.Fields{
			"Default":      s3DefaultKey,
			"Extension":    extension,
			"PrefixString": prefixString,
			"Unique":       versionKeyName,
		}).Debug("Created unique S3 keyname")
	}
	return versionKeyName, nil
}

// Upload a local file to S3.  Returns the full S3 URL to the file that was
// uploaded. If the target bucket does not have versioning enabled,
// this function will automatically make a new key to ensure uniqueness
func uploadLocalFileToS3(localPath string, s3ObjectKey string, ctx *workflowContext) (string, error) {

	// If versioning is enabled, use a stable name, otherwise use a name
	// that's dynamically created. By default assume that the bucket is
	// enabled for versioning
	if s3ObjectKey == "" {
		defaultS3KeyName := fmt.Sprintf("%s/%s", ctx.userdata.serviceName, filepath.Base(localPath))
		s3KeyName, s3KeyNameErr := versionAwareS3KeyName(defaultS3KeyName,
			ctx.context.s3BucketVersioningEnabled,
			ctx.logger)
		if nil != s3KeyNameErr {
			return "", errors.Wrapf(s3KeyNameErr, "Failed to create version aware S3 keyname")
		}
		s3ObjectKey = s3KeyName
	}

	s3URL := ""
	if ctx.userdata.noop {

		// Binary size
		filesize := int64(0)
		stat, statErr := os.Stat(localPath)
		if statErr == nil {
			filesize = stat.Size()
		}
		ctx.logger.WithFields(logrus.Fields{
			"Bucket": ctx.userdata.s3Bucket,
			"Key":    s3ObjectKey,
			"File":   filepath.Base(localPath),
			"Size":   humanize.Bytes(uint64(filesize)),
		}).Info(noopMessage("S3 upload"))
		s3URL = fmt.Sprintf("https://%s-s3.amazonaws.com/%s",
			ctx.userdata.s3Bucket,
			s3ObjectKey)
	} else {
		// Make sure we mark things for cleanup in case there's a problem
		ctx.registerFileCleanupFinalizer(localPath)
		// Then upload it
		uploadLocation, uploadURLErr := spartaS3.UploadLocalFileToS3(localPath,
			ctx.context.awsSession,
			ctx.userdata.s3Bucket,
			s3ObjectKey,
			ctx.logger)
		if nil != uploadURLErr {
			return "", errors.Wrapf(uploadURLErr, "Failed to upload local file to S3")
		}
		s3URL = uploadLocation
		ctx.registerRollback(spartaS3.CreateS3RollbackFunc(ctx.context.awsSession, uploadLocation))
	}
	return s3URL, nil
}

// Private - END
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// Workflow steps
////////////////////////////////////////////////////////////////////////////////

func showOptionalAWSUsageInfo(err error, logger *logrus.Logger) {
	if err == nil {
		return
	}
	userAWSErr, userAWSErrOk := err.(awserr.Error)
	if userAWSErrOk {
		if strings.Contains(userAWSErr.Error(), "could not find region configuration") {
			logger.Error("")
			logger.Error("Consider setting env.AWS_REGION, env.AWS_DEFAULT_REGION, or env.AWS_SDK_LOAD_CONFIG to resolve this issue.")
			logger.Error("See the documentation at https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/configuring-sdk.html for more information.")
			logger.Error("")
		}
	}
}

// Verify & cache the IAM rolename to ARN mapping
func verifyIAMRoles(ctx *workflowContext) (workflowStep, error) {
	defer recordDuration(time.Now(), "Verifying IAM roles", ctx)

	// The map is either a literal Arn from a pre-existing role name
	// or a gocf.RefFunc() value.
	// Don't verify them, just create them...
	ctx.logger.Info("Verifying IAM Lambda execution roles")
	ctx.context.lambdaIAMRoleNameMap = make(map[string]*gocf.StringExpr)
	iamSvc := iam.New(ctx.context.awsSession)

	// Assemble all the RoleNames and validate the inline IAMRoleDefinitions
	var allRoleNames []string
	for _, eachLambdaInfo := range ctx.userdata.lambdaAWSInfos {
		if eachLambdaInfo.RoleName != "" {
			allRoleNames = append(allRoleNames, eachLambdaInfo.RoleName)
		}
		// Custom resources?
		for _, eachCustomResource := range eachLambdaInfo.customResources {
			if eachCustomResource.roleName != "" {
				allRoleNames = append(allRoleNames, eachCustomResource.roleName)
			}
		}
		// Profiling enabled?
		if profileDecorator != nil {
			profileErr := profileDecorator(ctx.userdata.serviceName,
				eachLambdaInfo,
				ctx.userdata.s3Bucket,
				ctx.logger)
			if profileErr != nil {
				return nil, errors.Wrapf(profileErr, "Failed to call lambda profile decorator")
			}
		}

		// Validate the IAMRoleDefinitions associated
		if nil != eachLambdaInfo.RoleDefinition {
			logicalName := eachLambdaInfo.RoleDefinition.logicalName(ctx.userdata.serviceName, eachLambdaInfo.lambdaFunctionName())
			_, exists := ctx.context.lambdaIAMRoleNameMap[logicalName]
			if !exists {
				// Insert it into the resource creation map and add
				// the "Ref" entry to the hashmap
				ctx.context.cfTemplate.AddResource(logicalName,
					eachLambdaInfo.RoleDefinition.toResource(eachLambdaInfo.EventSourceMappings,
						eachLambdaInfo.Options,
						ctx.logger))

				ctx.context.lambdaIAMRoleNameMap[logicalName] = gocf.GetAtt(logicalName, "Arn")
			}
		}

		// And the custom resource IAMRoles as well...
		for _, eachCustomResource := range eachLambdaInfo.customResources {
			if nil != eachCustomResource.roleDefinition {
				customResourceLogicalName := eachCustomResource.roleDefinition.logicalName(ctx.userdata.serviceName,
					eachCustomResource.userFunctionName)

				_, exists := ctx.context.lambdaIAMRoleNameMap[customResourceLogicalName]
				if !exists {
					ctx.context.cfTemplate.AddResource(customResourceLogicalName,
						eachCustomResource.roleDefinition.toResource(nil,
							eachCustomResource.options,
							ctx.logger))
					ctx.context.lambdaIAMRoleNameMap[customResourceLogicalName] = gocf.GetAtt(customResourceLogicalName, "Arn")
				}
			}
		}
	}

	// Then check all the RoleName literals
	for _, eachRoleName := range allRoleNames {
		_, exists := ctx.context.lambdaIAMRoleNameMap[eachRoleName]
		if !exists {
			// Check the role
			params := &iam.GetRoleInput{
				RoleName: aws.String(eachRoleName),
			}
			ctx.logger.Debug("Checking IAM RoleName: ", eachRoleName)
			resp, err := iamSvc.GetRole(params)
			if err != nil {
				return nil, err
			}
			// Cache it - we'll need it later when we create the
			// CloudFormation template which needs the execution Arn (not role)
			ctx.context.lambdaIAMRoleNameMap[eachRoleName] = gocf.String(*resp.Role.Arn)
		}
	}
	ctx.logger.WithFields(logrus.Fields{
		"Count": len(ctx.context.lambdaIAMRoleNameMap),
	}).Info("IAM roles verified")

	return verifyAWSPreconditions, nil
}

// Verify that everything is setup in AWS before we start building things
func verifyAWSPreconditions(ctx *workflowContext) (workflowStep, error) {
	defer recordDuration(time.Now(), "Verifying AWS preconditions", ctx)

	// If this a NOOP, assume that versioning is not enabled
	if ctx.userdata.noop {
		ctx.logger.WithFields(logrus.Fields{
			"VersioningEnabled": false,
			"Bucket":            ctx.userdata.s3Bucket,
			"Region":            *ctx.context.awsSession.Config.Region,
		}).Info(noopMessage("S3 preconditions check"))
	} else if len(ctx.userdata.lambdaAWSInfos) != 0 {
		// We only need to check this if we're going to upload a ZIP, which
		// isn't always true in the case of a Step function...
		// Bucket versioning
		// Get the S3 bucket and see if it has versioning enabled
		isEnabled, versioningPolicyErr := spartaS3.BucketVersioningEnabled(ctx.context.awsSession,
			ctx.userdata.s3Bucket,
			ctx.logger)
		if nil != versioningPolicyErr {
			// If this is an error and suggests missing region, output some helpful error text
			return nil, versioningPolicyErr
		}
		ctx.logger.WithFields(logrus.Fields{
			"VersioningEnabled": isEnabled,
			"Bucket":            ctx.userdata.s3Bucket,
		}).Info("Checking S3 versioning")
		ctx.context.s3BucketVersioningEnabled = isEnabled
		if ctx.userdata.codePipelineTrigger != "" && !isEnabled {
			return nil, fmt.Errorf("s3 Bucket (%s) for CodePipeline trigger doesn't have a versioning policy enabled", ctx.userdata.s3Bucket)
		}
		// Bucket region should match region
		/*
			The name of the Amazon S3 bucket where the .zip file that contains your deployment package is stored. This bucket must reside in the same AWS Region that you're creating the Lambda function in. You can specify a bucket from another AWS account as long as the Lambda function and the bucket are in the same region.
		*/

		bucketRegion, bucketRegionErr := spartaS3.BucketRegion(ctx.context.awsSession,
			ctx.userdata.s3Bucket,
			ctx.logger)

		if bucketRegionErr != nil {
			return nil, fmt.Errorf("failed to determine region for %s. Error: %s",
				ctx.userdata.s3Bucket,
				bucketRegionErr)
		}
		ctx.logger.WithFields(logrus.Fields{
			"Bucket": ctx.userdata.s3Bucket,
			"Region": bucketRegion,
		}).Info("Checking S3 region")
		if bucketRegion != *ctx.context.awsSession.Config.Region {
			return nil, fmt.Errorf("region (%s) does not match bucket region (%s)",
				*ctx.context.awsSession.Config.Region,
				bucketRegion)
		}
		// Nothing else to do...
		ctx.logger.WithFields(logrus.Fields{
			"Region": bucketRegion,
		}).Debug("Confirmed S3 region match")
	}

	// If there are codePipeline environments defined, warn if they don't include
	// the same keysets
	if nil != codePipelineEnvironments {
		mapKeys := func(inboundMap map[string]string) []string {
			keys := make([]string, len(inboundMap))
			i := 0
			for k := range inboundMap {
				keys[i] = k
				i++
			}
			return keys
		}
		aggregatedKeys := make([][]string, len(codePipelineEnvironments))
		i := 0
		for _, eachEnvMap := range codePipelineEnvironments {
			aggregatedKeys[i] = mapKeys(eachEnvMap)
			i++
		}
		i = 0
		keysEqual := true
		for _, eachKeySet := range aggregatedKeys {
			j := 0
			for _, eachKeySetTest := range aggregatedKeys {
				if j != i {
					if !reflect.DeepEqual(eachKeySet, eachKeySetTest) {
						keysEqual = false
					}
				}
				j++
			}
			i++
		}
		if !keysEqual {
			// Setup an interface with the fields so that the log message
			fields := make(logrus.Fields, len(codePipelineEnvironments))
			for eachEnv, eachEnvMap := range codePipelineEnvironments {
				fields[eachEnv] = eachEnvMap
			}
			ctx.logger.WithFields(fields).Warn("CodePipeline environments do not define equivalent environment keys")
		}
	}

	return createPackageStep(), nil
}

// Build and package the application
func createPackageStep() workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		defer recordDuration(time.Now(), "Creating code bundle", ctx)

		// PreBuild Hook
		if ctx.userdata.workflowHooks != nil {
			preBuildErr := callWorkflowHook("PreBuild",
				ctx.userdata.workflowHooks.PreBuild,
				ctx.userdata.workflowHooks.PreBuilds,
				ctx)
			if nil != preBuildErr {
				return nil, preBuildErr
			}
		}
		sanitizedServiceName := sanitizedName(ctx.userdata.serviceName)
		buildErr := system.BuildGoBinary(ctx.userdata.serviceName,
			ctx.context.binaryName,
			ctx.userdata.useCGO,
			ctx.userdata.buildID,
			ctx.userdata.buildTags,
			ctx.userdata.linkFlags,
			ctx.userdata.noop,
			ctx.logger)
		if nil != buildErr {
			return nil, buildErr
		}
		// Cleanup the temporary binary
		defer func() {
			errRemove := os.Remove(ctx.context.binaryName)
			if nil != errRemove {
				ctx.logger.WithFields(logrus.Fields{
					"File":  ctx.context.binaryName,
					"Error": errRemove,
				}).Warn("Failed to delete binary")
			}
		}()

		// PostBuild Hook
		if ctx.userdata.workflowHooks != nil {
			postBuildErr := callWorkflowHook("PostBuild",
				ctx.userdata.workflowHooks.PostBuild,
				ctx.userdata.workflowHooks.PostBuilds,
				ctx)
			if nil != postBuildErr {
				return nil, postBuildErr
			}
		}
		tmpFile, err := system.TemporaryFile(ScratchDirectory,
			fmt.Sprintf("%s-code.zip", sanitizedServiceName))
		if err != nil {
			return nil, err
		}
		// Strip the local directory in case it's in there...
		ctx.logger.WithFields(logrus.Fields{
			"TempName": relativePath(tmpFile.Name()),
		}).Info("Creating code ZIP archive for upload")
		lambdaArchive := zip.NewWriter(tmpFile)

		// Archive Hook
		archiveErr := callArchiveHook(lambdaArchive, ctx)
		if nil != archiveErr {
			return nil, archiveErr
		}
		// Issue: https://github.com/mweagle/Sparta/issues/103. If the executable
		// bit isn't set, then AWS Lambda won't be able to fork the binary
		var fileHeaderAnnotator spartaZip.FileHeaderAnnotator
		if runtime.GOOS == "windows" || runtime.GOOS == "android" {
			fileHeaderAnnotator = func(header *zip.FileHeader) (*zip.FileHeader, error) {
				// Make the binary executable
				// Ref: https://github.com/aws/aws-lambda-go/blob/master/cmd/build-lambda-zip/main.go#L51
				header.CreatorVersion = 3 << 8
				header.ExternalAttrs = 0777 << 16
				return header, nil
			}
		}
		// File info for the binary executable
		readerErr := spartaZip.AnnotateAddToZip(lambdaArchive,
			ctx.context.binaryName,
			"",
			fileHeaderAnnotator,
			ctx.logger)
		if nil != readerErr {
			return nil, readerErr
		}
		archiveCloseErr := lambdaArchive.Close()
		if nil != archiveCloseErr {
			return nil, archiveCloseErr
		}
		tempfileCloseErr := tmpFile.Close()
		if nil != tempfileCloseErr {
			return nil, tempfileCloseErr
		}
		return createUploadStep(tmpFile.Name()), nil
	}
}

// Given the zipped binary in packagePath, upload the primary code bundle
// and optional S3 site resources iff they're defined.
func createUploadStep(packagePath string) workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		defer recordDuration(time.Now(), "Uploading code", ctx)

		var uploadTasks []*workTask
		if len(ctx.userdata.lambdaAWSInfos) != 0 {
			// We always upload the primary binary...
			uploadBinaryTask := func() workResult {
				logFilesize("Lambda code archive size", packagePath, ctx.logger)

				// Create the S3 key...
				zipS3URL, zipS3URLErr := uploadLocalFileToS3(packagePath, "", ctx)
				if nil != zipS3URLErr {
					return newTaskResult(nil, zipS3URLErr)
				}
				ctx.context.s3CodeZipURL = newS3UploadURL(zipS3URL)
				return newTaskResult(ctx.context.s3CodeZipURL, nil)
			}
			uploadTasks = append(uploadTasks, newWorkTask(uploadBinaryTask))
		} else {
			ctx.logger.Info("Bypassing S3 upload as no Lambda functions were provided")
		}

		// We might need to upload some other things...
		if nil != ctx.userdata.s3SiteContext.s3Site {
			uploadSiteTask := func() workResult {
				tempName := fmt.Sprintf("%s-S3Site.zip", ctx.userdata.serviceName)
				tmpFile, err := system.TemporaryFile(ScratchDirectory, tempName)
				if err != nil {
					return newTaskResult(nil,
						errors.Wrapf(err, "Failed to create temporary S3 site archive file"))
				}

				// Add the contents to the Zip file
				zipArchive := zip.NewWriter(tmpFile)
				absResourcePath, err := filepath.Abs(ctx.userdata.s3SiteContext.s3Site.resources)
				if nil != err {
					return newTaskResult(nil, errors.Wrapf(err, "Failed to get absolute filepath"))
				}
				// Ensure that the directory exists...
				_, existsErr := os.Stat(ctx.userdata.s3SiteContext.s3Site.resources)
				if existsErr != nil && os.IsNotExist(existsErr) {
					return newTaskResult(nil,
						errors.Wrapf(existsErr,
							"TheS3 Site resources directory (%s) does not exist",
							ctx.userdata.s3SiteContext.s3Site.resources))
				}

				ctx.logger.WithFields(logrus.Fields{
					"S3Key":      path.Base(tmpFile.Name()),
					"SourcePath": absResourcePath,
				}).Info("Creating S3Site archive")

				err = spartaZip.AddToZip(zipArchive, absResourcePath, absResourcePath, ctx.logger)
				if nil != err {
					return newTaskResult(nil, err)
				}
				errClose := zipArchive.Close()
				if errClose != nil {
					return newTaskResult(nil, errClose)
				}

				// Upload it & save the key
				s3SiteLambdaZipURL, s3SiteLambdaZipURLErr := uploadLocalFileToS3(tmpFile.Name(), "", ctx)
				if s3SiteLambdaZipURLErr != nil {
					return newTaskResult(nil,
						errors.Wrapf(s3SiteLambdaZipURLErr, "Failed to upload local file to S3"))
				}
				ctx.userdata.s3SiteContext.s3UploadURL = newS3UploadURL(s3SiteLambdaZipURL)
				return newTaskResult(ctx.userdata.s3SiteContext.s3UploadURL, nil)
			}
			uploadTasks = append(uploadTasks, newWorkTask(uploadSiteTask))

		}

		// Run it and figure out what happened
		p := newWorkerPool(uploadTasks, len(uploadTasks))
		_, uploadErrors := p.Run()

		if len(uploadErrors) > 0 {
			return nil, errors.Errorf("Encountered multiple errors during upload: %#v", uploadErrors)
		}
		return validateSpartaPostconditions(), nil
	}
}

// maximumStackOperationTimeout returns the timeout
// value to use for a stack operation based on the type
// of resources that it provisions. In general the timeout
// is short with an exception made for CloudFront
// distributions
func maximumStackOperationTimeout(template *gocf.Template, logger *logrus.Logger) time.Duration {
	stackOperationTimeout := 20 * time.Minute
	// If there is a CloudFront distributon in there then
	// let's give that a bit more time to settle down...In general
	// the initial CloudFront distribution takes ~30 minutes
	for _, eachResource := range template.Resources {
		if eachResource.Properties.CfnResourceType() == "AWS::CloudFront::Distribution" {
			stackOperationTimeout = 60 * time.Minute
			break
		}
	}
	logger.WithField("OperationTimeout", stackOperationTimeout).Debug("Computed operation timeout value")
	return stackOperationTimeout
}

// createCodePipelineTriggerPackage handles marshaling the template, zipping
// the config files in the package, and the
func createCodePipelineTriggerPackage(cfTemplateJSON []byte, ctx *workflowContext) (string, error) {
	tmpFile, err := system.TemporaryFile(ScratchDirectory, ctx.userdata.codePipelineTrigger)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to create temporary file for CodePipeline")
	}

	ctx.logger.WithFields(logrus.Fields{
		"PipelineName": tmpFile.Name(),
	}).Info("Creating pipeline archive")

	templateArchive := zip.NewWriter(tmpFile)
	ctx.logger.WithFields(logrus.Fields{
		"Path": tmpFile.Name(),
	}).Info("Creating CodePipeline archive")

	// File info for the binary executable
	zipEntryName := "cloudformation.json"
	bytesWriter, bytesWriterErr := templateArchive.Create(zipEntryName)
	if bytesWriterErr != nil {
		return "", bytesWriterErr
	}

	bytesReader := bytes.NewReader(cfTemplateJSON)
	written, writtenErr := io.Copy(bytesWriter, bytesReader)
	if nil != writtenErr {
		return "", writtenErr
	}
	ctx.logger.WithFields(logrus.Fields{
		"WrittenBytes": written,
		"ZipName":      zipEntryName,
	}).Debug("Archiving file")

	// If there is a codePipelineEnvironments defined, then we'll need to get all the
	// maps, marshal them to JSON, then add the JSON to the ZIP archive.
	if nil != codePipelineEnvironments {
		for eachEnvironment, eachMap := range codePipelineEnvironments {
			codePipelineParameters := map[string]interface{}{
				"Parameters": eachMap,
			}
			environmentJSON, environmentJSONErr := json.Marshal(codePipelineParameters)
			if nil != environmentJSONErr {
				ctx.logger.Error("Failed to Marshal CodePipeline environment: " + eachEnvironment)
				return "", environmentJSONErr
			}
			var envVarName = fmt.Sprintf("%s.json", eachEnvironment)

			// File info for the binary executable
			binaryWriter, binaryWriterErr := templateArchive.Create(envVarName)
			if binaryWriterErr != nil {
				return "", binaryWriterErr
			}
			_, writeErr := binaryWriter.Write(environmentJSON)
			if writeErr != nil {
				return "", writeErr
			}
		}
	}
	archiveCloseErr := templateArchive.Close()
	if nil != archiveCloseErr {
		return "", archiveCloseErr
	}
	tempfileCloseErr := tmpFile.Close()
	if nil != tempfileCloseErr {
		return "", tempfileCloseErr
	}
	// Leave it here...
	ctx.logger.WithFields(logrus.Fields{
		"File": filepath.Base(tmpFile.Name()),
	}).Info("Created CodePipeline archive")
	// The key is the name + the pipeline name
	return tmpFile.Name(), nil
}

// If the only detected changes to a stack are Lambda code updates,
// then update use the LAmbda API to update the code directly
// rather than waiting for CloudFormation
func applyInPlaceFunctionUpdates(ctx *workflowContext, templateURL string) (*cloudformation.Stack, error) {
	// Get the updates...
	awsCloudFormation := cloudformation.New(ctx.context.awsSession)
	changeSetRequestName := CloudFormationResourceName(fmt.Sprintf("%sInPlaceChangeSet", ctx.userdata.serviceName))
	changes, changesErr := spartaCF.CreateStackChangeSet(changeSetRequestName,
		ctx.userdata.serviceName,
		ctx.context.cfTemplate,
		templateURL,
		nil,
		awsCloudFormation,
		ctx.logger)
	if nil != changesErr {
		return nil, changesErr
	}
	if nil == changes || len(changes.Changes) <= 0 {
		return nil, fmt.Errorf("no changes detected")
	}
	updateCodeRequests := []*lambda.UpdateFunctionCodeInput{}
	invalidInPlaceRequests := []string{}
	for _, eachChange := range changes.Changes {
		resourceChange := eachChange.ResourceChange
		if *resourceChange.Action == "Modify" && *resourceChange.ResourceType == "AWS::Lambda::Function" {
			updateCodeRequest := &lambda.UpdateFunctionCodeInput{
				FunctionName: resourceChange.PhysicalResourceId,
				S3Bucket:     aws.String(ctx.userdata.s3Bucket),
				S3Key:        aws.String(ctx.context.s3CodeZipURL.keyName()),
			}
			if ctx.context.s3CodeZipURL != nil && ctx.context.s3CodeZipURL.version != "" {
				updateCodeRequest.S3ObjectVersion = aws.String(ctx.context.s3CodeZipURL.version)
			}
			updateCodeRequests = append(updateCodeRequests, updateCodeRequest)
		} else {
			invalidInPlaceRequests = append(invalidInPlaceRequests,
				fmt.Sprintf("%s for %s (ResourceType: %s)",
					*resourceChange.Action,
					*resourceChange.LogicalResourceId,
					*resourceChange.ResourceType))
		}
	}
	if len(invalidInPlaceRequests) != 0 {
		return nil, fmt.Errorf("unsupported in-place operations detected:\n\t%s", strings.Join(invalidInPlaceRequests, ",\n\t"))
	}

	ctx.logger.WithFields(logrus.Fields{
		"FunctionCount": len(updateCodeRequests),
	}).Info("Updating Lambda function code")
	ctx.logger.WithFields(logrus.Fields{
		"Updates": updateCodeRequests,
	}).Debug("Update requests")

	updateTaskMaker := func(lambdaSvc *lambda.Lambda, request *lambda.UpdateFunctionCodeInput) taskFunc {
		return func() workResult {
			_, updateResultErr := lambdaSvc.UpdateFunctionCode(request)
			return newTaskResult("", updateResultErr)
		}
	}
	inPlaceUpdateTasks := make([]*workTask,
		len(updateCodeRequests))
	awsLambda := lambda.New(ctx.context.awsSession)
	for eachIndex, eachUpdateCodeRequest := range updateCodeRequests {
		updateTask := updateTaskMaker(awsLambda, eachUpdateCodeRequest)
		inPlaceUpdateTasks[eachIndex] = newWorkTask(updateTask)
	}

	// Add the request to delete the change set...
	// TODO: add some retry logic in here to handle failures.
	deleteChangeSetTask := func() workResult {
		_, deleteChangeSetResultErr := spartaCF.DeleteChangeSet(ctx.userdata.serviceName,
			changeSetRequestName,
			awsCloudFormation)
		return newTaskResult("", deleteChangeSetResultErr)
	}
	inPlaceUpdateTasks = append(inPlaceUpdateTasks, newWorkTask(deleteChangeSetTask))
	p := newWorkerPool(inPlaceUpdateTasks, len(inPlaceUpdateTasks))
	_, asyncErrors := p.Run()
	if len(asyncErrors) != 0 {
		return nil, fmt.Errorf("failed to update function code: %v", asyncErrors)
	}
	// Describe the stack so that we can satisfy the contract with the
	// normal path using CloudFormation
	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(ctx.userdata.serviceName),
	}
	describeStackOutput, describeStackOutputErr := awsCloudFormation.DescribeStacks(describeStacksInput)
	if nil != describeStackOutputErr {
		return nil, describeStackOutputErr
	}
	return describeStackOutput.Stacks[0], nil
}

// applyCloudFormationOperation is responsible for taking the current template
// and applying that operation to the stack. It's where the in-place
// branch is applied, because at this point all the template
// mutations have been accumulated
func applyCloudFormationOperation(ctx *workflowContext) (workflowStep, error) {
	stackTags := map[string]string{
		SpartaTagBuildIDKey: ctx.userdata.buildID,
	}
	if len(ctx.userdata.buildTags) != 0 {
		stackTags[SpartaTagBuildTagsKey] = ctx.userdata.buildTags
	}

	// Generate the CF template...
	cfTemplate, err := json.Marshal(ctx.context.cfTemplate)
	if err != nil {
		ctx.logger.Error("Failed to Marshal CloudFormation template: ", err.Error())
		return nil, err
	}

	// Consistent naming of template
	sanitizedServiceName := sanitizedName(ctx.userdata.serviceName)
	templateName := fmt.Sprintf("%s-cftemplate.json", sanitizedServiceName)
	templateFile, templateFileErr := system.TemporaryFile(ScratchDirectory, templateName)
	if nil != templateFileErr {
		return nil, templateFileErr
	}
	_, writeErr := templateFile.Write(cfTemplate)
	if nil != writeErr {
		return nil, writeErr
	}
	errClose := templateFile.Close()
	if errClose != nil {
		return nil, errClose
	}
	// Log the template if needed
	if nil != ctx.context.templateWriter || ctx.logger.Level <= logrus.DebugLevel {
		templateBody := string(cfTemplate)
		formatted, formattedErr := json.MarshalIndent(templateBody, "", " ")
		if nil != formattedErr {
			return nil, formattedErr
		}
		ctx.logger.WithFields(logrus.Fields{
			"Body": string(formatted),
		}).Debug("CloudFormation template body")
		if nil != ctx.context.templateWriter {
			_, writeErr := io.WriteString(ctx.context.templateWriter,
				string(formatted))
			if writeErr != nil {
				return nil, errors.Wrapf(writeErr, "Failed to write template")
			}
		}
	}

	// If this isn't a codePipelineTrigger, then do that
	if ctx.userdata.codePipelineTrigger == "" {
		if ctx.userdata.noop {
			ctx.logger.WithFields(logrus.Fields{
				"Bucket":       ctx.userdata.s3Bucket,
				"TemplateName": templateName,
			}).Info(noopMessage("Stack creation"))
		} else {
			// Dump the template to a file, then upload it...
			uploadURL, uploadURLErr := uploadLocalFileToS3(templateFile.Name(), "", ctx)
			if nil != uploadURLErr {
				return nil, uploadURLErr
			}

			// If we're supposed to be inplace, then go ahead and try that
			var stack *cloudformation.Stack
			var stackErr error
			if ctx.userdata.inPlace {
				stack, stackErr = applyInPlaceFunctionUpdates(ctx, uploadURL)
			} else {
				operationTimeout := maximumStackOperationTimeout(ctx.context.cfTemplate, ctx.logger)
				// Regular update, go ahead with the CloudFormation changes
				stack, stackErr = spartaCF.ConvergeStackState(ctx.userdata.serviceName,
					ctx.context.cfTemplate,
					uploadURL,
					stackTags,
					ctx.transaction.startTime,
					operationTimeout,
					ctx.context.awsSession,
					"▬",
					dividerLength,
					ctx.logger)
			}
			if nil != stackErr {
				return nil, stackErr
			}
			ctx.logger.WithFields(logrus.Fields{
				"StackName":    *stack.StackName,
				"StackId":      *stack.StackId,
				"CreationTime": *stack.CreationTime,
			}).Info("Stack provisioned")
		}
	} else {
		ctx.logger.Info("Creating pipeline package")

		ctx.registerFileCleanupFinalizer(templateFile.Name())
		_, urlErr := createCodePipelineTriggerPackage(cfTemplate, ctx)
		if nil != urlErr {
			return nil, urlErr
		}
	}
	return nil, nil
}

func verifyLambdaPreconditions(lambdaAWSInfo *LambdaAWSInfo, logger *logrus.Logger) error {

	return nil
}

func validateSpartaPostconditions() workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		validateErrs := make([]error, 0)

		requiredEnvVars := []string{envVarDiscoveryInformation,
			envVarLogLevel}

		// Verify that all Lambda functions have discovery information
		for eachResourceID, eachResourceDef := range ctx.context.cfTemplate.Resources {
			switch typedResource := eachResourceDef.Properties.(type) {
			case *gocf.LambdaFunction:
				if typedResource.Environment == nil {
					validateErrs = append(validateErrs,
						errors.Errorf("Lambda function %s does not include environment info", eachResourceID))
				} else {
					vars, varsOk := typedResource.Environment.Variables.(map[string]interface{})
					if !varsOk {
						validateErrs = append(validateErrs,
							errors.Errorf("Lambda function %s environment vars are unsupported type: %T",
								eachResourceID,
								typedResource.Environment.Variables))
					} else {
						for _, eachKey := range requiredEnvVars {
							_, exists := vars[eachKey]
							if !exists {
								validateErrs = append(validateErrs,
									errors.Errorf("Lambda function %s environment does not include key: %s",
										eachResourceID,
										eachKey))
							}
						}
					}
				}
			}
		}
		if len(validateErrs) != 0 {
			return nil, errors.Errorf("Problems validating template contents: %v", validateErrs)
		}
		return ensureCloudFormationStack(), nil
	}
}

// ensureCloudFormationStack is responsible for
func ensureCloudFormationStack() workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		msg := "Ensuring CloudFormation stack"
		if ctx.userdata.inPlace {
			msg = "Updating Lambda function code "
		}
		defer recordDuration(time.Now(), msg, ctx)

		// PreMarshall Hook
		if ctx.userdata.workflowHooks != nil {
			preMarshallErr := callWorkflowHook("PreMarshall",
				ctx.userdata.workflowHooks.PreMarshall,
				ctx.userdata.workflowHooks.PreMarshalls,
				ctx)
			if nil != preMarshallErr {
				return nil, preMarshallErr
			}
		}

		// Add the "Parameters" to the template...
		if nil != codePipelineEnvironments {
			ctx.context.cfTemplate.Parameters = make(map[string]*gocf.Parameter)
			for _, eachEnvironment := range codePipelineEnvironments {
				for eachKey := range eachEnvironment {
					ctx.context.cfTemplate.Parameters[eachKey] = &gocf.Parameter{
						Type:    "String",
						Default: "",
					}
				}
			}
		}
		for _, eachEntry := range ctx.userdata.lambdaAWSInfos {
			verifyErr := verifyLambdaPreconditions(eachEntry, ctx.logger)
			if verifyErr != nil {
				return nil, verifyErr
			}
			annotateCodePipelineEnvironments(eachEntry, ctx.logger)

			err := eachEntry.export(ctx.userdata.serviceName,
				ctx.userdata.s3Bucket,
				codeZipKey(ctx.context.s3CodeZipURL),
				codeZipVersion(ctx.context.s3CodeZipURL),
				ctx.userdata.buildID,
				ctx.context.lambdaIAMRoleNameMap,
				ctx.context.cfTemplate,
				ctx.context.workflowHooksContext,
				ctx.logger)
			if nil != err {
				return nil, err
			}
		}
		// If there's an API gateway definition, include the resources that provision it. Since this export will likely
		// generate outputs that the s3 site needs, we'll use a temporary outputs accumulator, pass that to the S3Site
		// if it's defined, and then merge it with the normal output map.
		apiGatewayTemplate := gocf.NewTemplate()

		if nil != ctx.userdata.api {
			err := ctx.userdata.api.Marshal(
				ctx.userdata.serviceName,
				ctx.context.awsSession,
				ctx.userdata.s3Bucket,
				codeZipKey(ctx.context.s3CodeZipURL),
				codeZipVersion(ctx.context.s3CodeZipURL),
				ctx.context.lambdaIAMRoleNameMap,
				apiGatewayTemplate,
				ctx.userdata.noop,
				ctx.logger)
			if nil == err {
				safeMergeErrs := gocc.SafeMerge(apiGatewayTemplate,
					ctx.context.cfTemplate)
				if len(safeMergeErrs) != 0 {
					err = errors.Errorf("APIGateway template merge failed: %v", safeMergeErrs)
				}
			}
			if nil != err {
				return nil, errors.Wrapf(err, "APIGateway template export failed")
			}
		}

		// Service decorator?
		// This is run before the S3 Site in case the decorators
		// need to publish data to the MANIFEST for the site
		serviceDecoratorErr := callServiceDecoratorHook(ctx)
		if serviceDecoratorErr != nil {
			return nil, serviceDecoratorErr
		}

		// Discovery info on a per-function basis
		for _, eachEntry := range ctx.userdata.lambdaAWSInfos {
			_, annotateErr := annotateDiscoveryInfo(eachEntry, ctx.context.cfTemplate, ctx.logger)
			if annotateErr != nil {
				return nil, annotateErr
			}
			_, annotateErr = annotateBuildInformation(eachEntry,
				ctx.context.cfTemplate,
				ctx.userdata.buildID,
				ctx.logger)
			if annotateErr != nil {
				return nil, annotateErr
			}
			// Any custom resources? These may also need discovery info
			// so that they can self-discover the stack name
			for _, eachCustomResource := range eachEntry.customResources {
				discoveryInfo, discoveryInfoErr := discoveryInfoForResource(eachCustomResource.logicalName(),
					nil)
				if discoveryInfoErr != nil {
					return nil, discoveryInfoErr
				}
				ctx.logger.WithFields(logrus.Fields{
					"Discovery": discoveryInfo,
					"Resource":  eachCustomResource.logicalName(),
				}).Info("Annotating discovery info for custom resource")

				// Update the env map
				eachCustomResource.options.Environment[envVarDiscoveryInformation] = discoveryInfo
			}
		}
		// If there's a Site defined, include the resources the provision it
		if nil != ctx.userdata.s3SiteContext.s3Site {
			exportErr := ctx.userdata.s3SiteContext.s3Site.export(ctx.userdata.serviceName,
				ctx.context.binaryName,
				ctx.userdata.s3Bucket,
				codeZipKey(ctx.context.s3CodeZipURL),
				ctx.userdata.s3SiteContext.s3UploadURL.keyName(),
				apiGatewayTemplate.Outputs,
				ctx.context.lambdaIAMRoleNameMap,
				ctx.context.cfTemplate,
				ctx.logger)
			if exportErr != nil {
				return nil, errors.Wrapf(exportErr, "Failed to export S3 site")
			}
		}

		// PostMarshall Hook
		if ctx.userdata.workflowHooks != nil {
			postMarshallErr := callWorkflowHook("PostMarshall",
				ctx.userdata.workflowHooks.PostMarshall,
				ctx.userdata.workflowHooks.PostMarshalls,
				ctx)
			if nil != postMarshallErr {
				return nil, postMarshallErr
			}
		}
		// Last step, run the annotation steps to patch
		// up any references that depends on the entire
		// template being constructed
		_, annotateErr := annotateMaterializedTemplate(ctx.userdata.lambdaAWSInfos,
			ctx.context.cfTemplate,
			ctx.logger)
		if annotateErr != nil {
			return nil, errors.Wrapf(annotateErr,
				"Failed to perform final template annotations")
		}

		// validations?
		if ctx.userdata.workflowHooks != nil {
			validationErr := callValidationHooks(ctx.userdata.workflowHooks.Validators,
				ctx.context.cfTemplate,
				ctx)
			if validationErr != nil {
				return nil, validationErr
			}
		}

		// Do the operation!
		return applyCloudFormationOperation(ctx)
	}
}

// Provision compiles, packages, and provisions (either via create or update) a Sparta application.
// The serviceName is the service's logical
// identify and is used to determine create vs update operations.  The compilation options/flags are:
//
// 	TAGS:         -tags lambdabinary
// 	ENVIRONMENT:  GOOS=linux GOARCH=amd64
//
// The compiled binary is packaged with a NodeJS proxy shim to manage AWS Lambda setup & invocation per
// http://docs.aws.amazon.com/lambda/latest/dg/authoring-function-in-nodejs.html
//
// The two files are ZIP'd, posted to S3 and used as an input to a dynamically generated CloudFormation
// template (http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/Welcome.html)
// which creates or updates the service state.
//
func Provision(noop bool,
	serviceName string,
	serviceDescription string,
	lambdaAWSInfos []*LambdaAWSInfo,
	api APIGateway,
	site *S3Site,
	s3Bucket string,
	useCGO bool,
	inPlaceUpdates bool,
	buildID string,
	codePipelineTrigger string,
	buildTags string,
	linkerFlags string,
	templateWriter io.Writer,
	workflowHooks *WorkflowHooks,
	logger *logrus.Logger) error {

	err := validateSpartaPreconditions(lambdaAWSInfos, logger)
	if nil != err {
		return errors.Wrapf(err, "Failed to validate preconditions")
	}
	startTime := time.Now()

	ctx := &workflowContext{
		logger: logger,
		userdata: userdata{
			noop:               noop,
			useCGO:             useCGO,
			inPlace:            inPlaceUpdates,
			buildID:            buildID,
			buildTags:          buildTags,
			linkFlags:          linkerFlags,
			serviceName:        serviceName,
			serviceDescription: serviceDescription,
			lambdaAWSInfos:     lambdaAWSInfos,
			api:                api,
			s3Bucket:           s3Bucket,
			s3SiteContext: &s3SiteContext{
				s3Site: site,
			},
			codePipelineTrigger: codePipelineTrigger,
			workflowHooks:       workflowHooks,
		},
		context: provisionContext{
			cfTemplate:                gocf.NewTemplate(),
			s3BucketVersioningEnabled: false,
			awsSession:                spartaAWS.NewSession(logger),
			workflowHooksContext:      make(map[string]interface{}),
			templateWriter:            templateWriter,
			binaryName:                SpartaBinaryName,
		},
		transaction: transaction{
			startTime: time.Now(),
		},
	}
	ctx.context.cfTemplate.Description = serviceDescription

	// Update the context iff it exists
	if nil != workflowHooks && nil != workflowHooks.Context {
		for eachKey, eachValue := range workflowHooks.Context {
			ctx.context.workflowHooksContext[eachKey] = eachValue
		}
	}

	ctx.logger.WithFields(logrus.Fields{
		"BuildID":             buildID,
		"NOOP":                noop,
		"Tags":                ctx.userdata.buildTags,
		"CodePipelineTrigger": ctx.userdata.codePipelineTrigger,
		"InPlaceUpdates":      ctx.userdata.inPlace,
	}).Info("Provisioning service")

	if len(lambdaAWSInfos) <= 0 {
		// Warning? Maybe it's just decorators?
		if ctx.userdata.workflowHooks == nil {
			return errors.New("No lambda functions provided to Sparta.Provision()")
		}
		ctx.logger.Warn("No lambda functions provided to Sparta.Provision()")
	}

	// Start the workflow
	for step := verifyIAMRoles; step != nil; {
		next, err := step(ctx)
		if err != nil {
			showOptionalAWSUsageInfo(err, ctx.logger)

			ctx.rollback()
			// Workflow step?
			return errors.Wrapf(err, "Failed to provision service")
		}

		if next == nil {
			summaryLine := fmt.Sprintf("%s Summary", ctx.userdata.serviceName)
			ctx.logger.Info(headerDivider)
			ctx.logger.Info(summaryLine)
			ctx.logger.Info(headerDivider)
			for _, eachEntry := range ctx.transaction.stepDurations {
				ctx.logger.WithFields(logrus.Fields{
					"Duration (s)": fmt.Sprintf("%.f", eachEntry.duration.Seconds()),
				}).Info(eachEntry.name)
			}
			elapsed := time.Since(startTime)
			ctx.logger.WithFields(logrus.Fields{
				"Duration (s)": fmt.Sprintf("%.f", elapsed.Seconds()),
			}).Info("Total elapsed time")
			break
		} else {
			step = next
		}
	}
	// When we're done, execute any finalizers
	if nil != ctx.transaction.finalizerFunctions {
		ctx.logger.WithFields(logrus.Fields{
			"FinalizerCount": len(ctx.transaction.finalizerFunctions),
		}).Debug("Invoking finalizer functions")
		for _, eachFinalizer := range ctx.transaction.finalizerFunctions {
			eachFinalizer(ctx.logger)
		}
	}
	return nil
}
