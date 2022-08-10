// go-lambda-builder
//
// Usage:
//
//     builder \
//         -profile=kk \
//         -bucket=kesav-go-lambda-builder-test \
//         -unsigned-prefix=test/unsigned \
//         -staging-prefix=test/staging \
//         -signed-prefix=test/signed \
//         -signing-profile=main \
//         -folders=testLambda1,testLambda2 \
//         -no-upload \
//         -no-sign \
//         -no-copy-signed \
//         -no-update-functions \
//         -force
//
// TODO(kesav): make the flags look like this:
//
//     builder \
//         -chdir=test/lambdas \
//         -region=us-west-2 \
//         -profile=kk \
//         -unsigned-bucket-versioning-enabled \
//         -unsigned-bucket=kesav-go-lambda-builder-test \
//         -unsigned-prefix=test/unsigned \
//         -staging-bucket=kesav-go-lambda-builder-test \
//         -staging-prefix=test/staging \
//         -signed-bucket=kesav-go-lambda-builder-test \
//         -signed-prefix=test/signed \
//         -signing-profile=test_signer \
//         -include=testLambda1,testLambda2 \
//         -exclude=internal \
//         -no-upload \
//         -no-sign \
//         -no-copy-signed \
//         -no-update-functions \
//         -force
//
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/signer"
)

// required
var bucketFlag = flag.String("bucket", "", "Which bucket to use.")
var unsignedPrefixFlag = flag.String("unsigned-prefix", "", "Where to upload unsigned deployment packages.")
var stagingPrefixFlag = flag.String("staging-prefix", "", "Where to upload signed deployment packages for staging.")
var signedPrefixFlag = flag.String("signed-prefix", "", "Where to upload unsigned deployment packages for consumption.")
var signingProfileFlag = flag.String("signing-profile", "", "Which profile to use to sign deployment packages.")

// optional
var regionFlag = flag.String("region", "", "Which AWS region to use.")
var profileFlag = flag.String("profile", "", "Which AWS profile to use.")
var foldersFlag = flag.String("folders", "", "Which folders to deploy.")
var forceFlag = flag.Bool("force", false, "Deploy even if signed deployment package is up-to-date.")
var noUploadFlag = flag.Bool("no-upload", false, "Do not upload unsigned deployment packages to S3.")
var noSignFlag = flag.Bool("no-sign", false, "Do not run any signing jobs.")
var noCopySignedFlag = flag.Bool("no-copy-signed", false, "Do not copy signed deployment packages to signed prefix.")
var noUpdateFunctionsFlag = flag.Bool("no-update-functions", false, "Do not update Lambda functions.")
var instanceFlag = flag.Int("instance", 0, "Which instance this builder is.")
var numInstancesFlag = flag.Int("num-instances", 0, "Number of instances running.")

// TODO(kesav): look into ClientRequestToken
// TODO(kesav): check out https://aws.amazon.com/blogs/compute/migrating-aws-lambda-functions-to-arm-based-aws-graviton2-processors/
// TODO(kesav): assign each step a color so it's easier to tell the overall progress
// TODO(kesav): check out the s3 upload manager https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/feature/s3/manager#Uploader
// TODO(kesav): add flags for unsigned-bucket, staging-bucket, and signed-bucket
// TODO(kesav): make signing-profile optional, and don't run a signer job if not passed in
// TODO(kesav): do not require bucket versioning to be enabled
// TODO(kesav): record and print durations for every step
// TODO(kesav): change format of timer to 0m0s000ms
// TODO(kesav): read options from ~/.config/go-lambda-builder/config.hcl
//
// if you run two zips on the same input, the hashes of the outputs will be the same
//
// if you run two signing jobs on the same input, the hashes of the outputs will be different
//
// no need to use upx
// default is -7
//
// command                | time | compression ratio
//
// upx main               | 3s   | 52.49%
// upx --brute main       | 229s | 43.04%
// upx --ultra-brute main | 239s | 42.99%
//
// size of unsigned deployment package without upx | 6.04 M
// size of unsigned deployment package with upx -7 | 5.82 M
//
func main() {
	timer := newTimer()

	flag.Parse()

	if bucketFlag == nil {
		panic(`Flag "bucket" is required.`)
	}
	if unsignedPrefixFlag == nil {
		panic(`Flag "unsigned-prefix" is required.`)
	}
	if stagingPrefixFlag == nil {
		panic(`Flag "staging-prefix" is required.`)
	}
	if signedPrefixFlag == nil {
		panic(`Flag "signed-prefix" is required.`)
	}
	if signingProfileFlag == nil {
		panic(`Flag "signing-profile" is required.`)
	}

	noUpdateFunctions := *noUpdateFunctionsFlag
	force := *forceFlag

	allFolders, err := lambdaFolders()
	if err != nil {
		panic(err)
	}
	folders := []string{}
	// if the folders flag is passed in, only accept the folders that exist
	if foldersFlag != nil && *foldersFlag != "" {
		for _, s := range strings.Split(*foldersFlag, ",") {
			if !contains(allFolders, s) {
				fmt.Printf("Lambda folders: %s.\n", strings.Join(allFolders, ", "))
				panic(fmt.Sprintf(`Argument "%s" is not a Lambda folder.`, s))
			}
			folders = append(folders, s)
		}
	} else {
		folders = allFolders
	}

	if instanceFlag != nil && numInstancesFlag != nil {
		fmt.Printf("Running instance %d of %d.\n\n", *instanceFlag, *numInstancesFlag)
		var chunks [][]string
		chunkSize := (len(folders) + *numInstancesFlag - 1) / *numInstancesFlag
		for i := 0; i < len(folders); i += chunkSize {
			end := i + chunkSize
			if end > len(folders) {
				end = len(folders)
			}
			chunks = append(chunks, folders[i:end])
		}
		folders = chunks[*instanceFlag]
	}

	fmt.Printf("Deploying %s.\n\n", strings.Join(folders, ", "))

	environ := os.Environ()
	environ = append(environ, "GOOS=linux")
	environ = append(environ, "GOARCH=amd64")
	environ = append(environ, "CGO_ENABLED=0")

	var opts []func(*config.LoadOptions) error
	if regionFlag != nil {
		opts = append(opts, config.WithRegion(*regionFlag))
	}
	if profileFlag != nil {
		opts = append(opts, config.WithSharedConfigProfile(*profileFlag))
	}
	cfg, err := config.LoadDefaultConfig(context.TODO(), opts...)
	if err != nil {
		panic(err)
	}

	s3Client := s3.NewFromConfig(cfg)

	signerClient := signer.NewFromConfig(cfg)
	signingJobWaiter := signer.NewSuccessfulSigningJobWaiter(
		signerClient,
		func(o *signer.SuccessfulSigningJobWaiterOptions) {
			o.MinDelay = 2
			o.MaxDelay = 10
		})

	lambdaClient := lambda.NewFromConfig(cfg)
	functionUpdatedWaiter := lambda.NewFunctionUpdatedV2Waiter(
		lambdaClient,
		func(o *lambda.FunctionUpdatedV2WaiterOptions) {
			o.MinDelay = 3
			o.MaxDelay = 10
		})

	d := &data{
		// context to use in api calls
		ctx: context.TODO(),
		// flags
		noUpload:          *noUploadFlag,
		noSigningJobs:     *noSignFlag,
		noCopySigned:      *noCopySignedFlag,
		noUpdateFunctions: noUpdateFunctions,
		force:             force,
		// environment variables to pass to go build
		environ: environ,
		// s3 config
		s3:             s3Client,
		bucket:         *bucketFlag,
		unsignedPrefix: *unsignedPrefixFlag,
		stagingPrefix:  *stagingPrefixFlag,
		signedPrefix:   *signedPrefixFlag,
		// signer config
		signer:           signerClient,
		signingProfile:   *signingProfileFlag,
		signingJobWaiter: signingJobWaiter,
		// lambda config
		lambda:                lambdaClient,
		functionUpdatedWaiter: functionUpdatedWaiter,
	}

	type result struct {
		string
		error
	}
	results := make(chan result, len(folders))
	for _, folder := range folders {
		go func(folder string) {
			results <- result{folder, d.run(folder)}
		}(folder)
	}

	numResults := 0
	failures := []string{}
	for result := range results {
		numResults++
		if result.error != nil {
			failures = append(failures, result.string)
		}
		if numResults == len(folders) {
			close(results)
		}
	}

	fmt.Printf("\nTook %s.\n\n", timer().String())

	if len(failures) != 0 {
		sort.Strings(failures)
		panic(strings.Join(failures, ", "))
	}
}

func lambdaFolders() ([]string, error) {
	matches, err := filepath.Glob("*/*.go")
	if err != nil {
		return nil, err
	}
	folders := []string{}
	for _, match := range matches {
		dir, _ := filepath.Split(match)
		dir = dir[:len(dir)-1]
		if dir == "internal" {
			continue
		}
		folders = append(folders, dir)
	}
	sort.Strings(folders)
	return folders, nil
}

// Returns true if the slice contains the string.
func contains(strs []string, match string) bool {
	for _, str := range strs {
		if str == match {
			return true
		}
	}
	return false
}

// Returns a function that returns a string.
// Expects duration to be less than one hour.
//
//     fmt.Printf("%s | Doing something.\n", folder)
//     t := newTimer()
//     err = doSomething(folder)
//     if err != nil {
//         fmt.Printf("%s | Failed to do something: %s\n", folder, err.Error())
//         return
//     }
//     fmt.Printf("%s | Did something. Took %s.\n", folder, t())
//
func newTimer() func() time.Duration {
	startTime := time.Now()
	return func() time.Duration {
		return time.Since(startTime)
	}
}
