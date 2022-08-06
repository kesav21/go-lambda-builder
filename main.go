// go-lambda-builder
//
//     ./builder \
//         -bucket=kesav-go-lambda-builder-test \
//         -signing-prefix=test/unsigned \
//         -staging-prefix=test/staging \
//         -signed-prefix=test/signed \
//         -signing-profile=test_signer \
//         -include=testLambda1,testLambda2 \
//         -exclude=internal \
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
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/signer"
)

var bucketFlag = flag.String("bucket", "", "Which bucket to use.")
var unsignedPrefixFlag = flag.String("unsigned-prefix", "", "Where to upload unsigned deployment packages.")
var stagingPrefixFlag = flag.String("staging-prefix", "", "Where to upload signed deployment packages for staging.")
var signedPrefixFlag = flag.String("signed-prefix", "", "Where to upload unsigned deployment packages for consumption.")
var signingProfileFlag = flag.String("signing-profile", "", "Which profile to use to sign deployment packages.")
var foldersFlag = flag.String("folders", "", "Which folders to deploy.")
var forceFlag = flag.Bool("force", false, "Deploy even if signed deployment package is up-to-date.")
var noUpdateFunctionsFlag = flag.Bool("no-update-functions", false, "Do not update Lambda functions.")

// var noPackFlag = flag.String("no-pack", "", "Which folders to deploy.")
// var aggressivePackFlag = flag.String("aggressive-pack", "", "Which folders to deploy.")

// This must be run from the lambdas folder
//
// TODO(kesav): use upx to make binaries smaller
// TODO(kesav): look into ClientRequestToken
// TODO(kesav): check out https://aws.amazon.com/blogs/compute/migrating-aws-lambda-functions-to-arm-based-aws-graviton2-processors/
// TODO(kesav): assign each step a color so it's easier to tell the overall progress
// TODO(kesav): check out the s3 upload manager https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/feature/s3/manager#Uploader
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
		fmt.Printf("Only deploying %s.\n\n", strings.Join(folders, ", "))
	} else {
		folders = allFolders
		fmt.Printf("Deploying all folders.\n")
	}

	environ := os.Environ()
	environ = append(environ, "GOOS=linux")
	environ = append(environ, "GOARCH=amd64")
	environ = append(environ, "CGO_ENABLED=0")

	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion("us-east-1"))
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

	wg := sync.WaitGroup{}
	for _, folder := range folders {
		wg.Add(1)
		go func(folder string) {
			defer wg.Done()
			d.run(folder)
		}(folder)
	}
	wg.Wait()

	fmt.Printf("\n")
	fmt.Printf("Took %s.\n", timer())
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
	sort.Sort(sort.StringSlice(folders))
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
func newTimer() func() string {
	startTime := time.Now()
	return func() string {
		duration := time.Now().Sub(startTime)
		minutes := int(duration.Minutes())
		seconds := int(duration.Seconds()) % 60
		if minutes == 0 {
			return fmt.Sprintf("%d seconds", seconds)
		}
		return fmt.Sprintf("%d minutes and %d seconds", minutes, seconds)
	}
}
