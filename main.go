package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/signer"
	signerTypes "github.com/aws/aws-sdk-go-v2/service/signer/types"
)

var envFlag = flag.String("env", "test", `Which enviroment to target. Must be "test" or "prod".`)
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
// if you run two signing jobs on the same input, the hashes of the outputs will be
// different
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
	t0 := newTimer()

	// err := os.Chdir("lambdas")
	// if err != nil {
	// 	panic(err)
	// }

	flag.Parse()

	noUpdateFunctions := *noUpdateFunctionsFlag
	force := *forceFlag

	env := "test"
	if *envFlag != "test" && *envFlag != "prod" {
		env = "test"
	}

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

	bucket := fmt.Sprintf("flipx-binaries-%s-us-east-1", env)
	unsignedPrefix := fmt.Sprintf("%s/unsigned", env)
	stagingPrefix := fmt.Sprintf("%s/staging", env)
	signedPrefix := fmt.Sprintf("%s/signed", env)

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
	lambdaClient := lambda.NewFromConfig(cfg)
	signingJobWaiter := signer.NewSuccessfulSigningJobWaiter(
		signerClient,
		func(o *signer.SuccessfulSigningJobWaiterOptions) {
			o.MinDelay = 2
			o.MaxDelay = 10
		})
	functionUpdatedWaiter := lambda.NewFunctionUpdatedV2Waiter(
		lambdaClient,
		func(o *lambda.FunctionUpdatedV2WaiterOptions) {
			o.MinDelay = 3
			o.MaxDelay = 10
		})

	signingProfile := env + "_signer"

	// err = cleanBinaries(s3Client, bucket, unsignedPrefix, stagingPrefix)
	// if err != nil {
	// 	panic(err)
	// }

	// hasLinter := true
	// _, err = exec.LookPath("golangci-lint")
	// if err != nil {
	// 	hasLinter = false
	// }

	// matches, err := filepath.Glob("/usr/local/go/bin")
	// if err != nil {
	// 	panic("kk")
	// }
	// for _, match := range matches {
	// 	fmt.Printf("%s\n", match)
	// }

	// fmt.Printf("%s\n", os.Getenv("PATH"))

	var wg sync.WaitGroup
	for _, folder := range folders {
		wg.Add(1)
		go func(folder string) {
			defer wg.Done()
			//
			executablePath := fmt.Sprintf("/tmp/%s", folder)
			unsignedKey := fmt.Sprintf("%s/%s.zip", unsignedPrefix, folder)
			signedKey := fmt.Sprintf("%s/%s.zip", signedPrefix, folder)
			//
			fmt.Printf("%s | Building executable.\n", folder)
			t2 := newTimer()
			err = buildExecutable(executablePath, folder, environ)
			if err != nil {
				fmt.Printf("%s | Failed to build executable: %s.\n", folder, err.Error())
				return
			}
			defer os.Remove(executablePath)
			fmt.Printf("%s | Built executable. Took %s.\n", folder, t2())
			//
			fmt.Printf("%s | Zipping executable.\n", folder)
			t3 := newTimer()
			unsignedR, err := zipExecutable(executablePath)
			if err != nil {
				fmt.Printf("%s | Failed to zip executable: %s.\n", folder, err.Error())
				return
			}
			fmt.Printf("%s | Zipped executable. Took %s.\n", folder, t3())
			//
			unsignedR, unsignedR1 := duplicateR(unsignedR)
			unsignedR, unsignedR2 := duplicateR(unsignedR)
			fmt.Printf("%s | Size of unsigned deployment package: %.2f M.\n", folder, float64(lenR(unsignedR))/1000000)
			//
			fmt.Printf("%s | Hashing unsigned deployment package.\n", folder)
			t4 := newTimer()
			unsignedHash, err := hashR(unsignedR1)
			if err != nil {
				fmt.Printf("%s | Failed to hash unsigned deployment package: %s.\n", folder, err.Error())
				return
			}
			fmt.Printf("%s | Hashed unsigned deployment package: %s. Took %s.\n", folder, unsignedHash, t4())
			//
			if !force {
				fmt.Printf("%s | Checking if previous deployment package is up to date.\n", folder)
				t5 := newTimer()
				previous, isUpToDate := isUpToDate(s3Client, bucket, signedKey, unsignedHash)
				if isUpToDate {
					fmt.Printf("%s | Deployment package is up to date, stopping.\n", folder)
					return
				}
				fmt.Printf("%s | Deployment package is out of date, proceeding: %s. Took %s.\n", folder, previous, t5())
			}
			//
			fmt.Printf("%s | Pushing unsigned deployment package to S3.\n", folder)
			t6 := newTimer()
			objectVersion, err := putObject(s3Client, bucket, unsignedKey, unsignedR2, nil)
			if err != nil {
				fmt.Printf("%s | Failed to upload binary: %s\n", folder, err.Error())
				return
			}
			defer deleteObject(s3Client, bucket, unsignedKey)
			fmt.Printf("%s | Pushed unsigned deployment package to S3 with version ID: %s. Took %s.\n", folder, objectVersion, t6())
			//
			fmt.Printf("%s | Starting signing job.\n", folder)
			t7 := newTimer()
			jobId, err := startSigningJob(
				signerClient,
				signingProfile,
				bucket,
				unsignedKey,
				objectVersion,
				stagingPrefix,
			)
			if err != nil {
				fmt.Printf("%s | Failed to start signing job: %s\n", folder, err.Error())
				return
			}
			fmt.Printf("%s | Started signing job with id: %s. Took %s.\n", folder, jobId, t7())
			stagingKey := stagingPrefix + "/" + jobId + ".zip"
			//
			fmt.Printf("%s | Waiting for signing job to complete.\n", folder)
			t8 := newTimer()
			err = waitForSigningJob(signingJobWaiter, jobId)
			if err != nil {
				fmt.Printf("%s | Failed to wait for signing job to complete: %s\n", folder, err.Error())
				return
			}
			defer deleteObject(s3Client, bucket, stagingKey)
			fmt.Printf("%s | Signing job is complete. Took %s.\n", folder, t8())
			//
			fmt.Printf("%s | Downloading signed deployment package.\n", folder)
			t9 := newTimer()
			signedR, err := getObject(s3Client, bucket, stagingKey)
			if err != nil {
				fmt.Printf("%s | Failed to download signed deployment package: %s\n", folder, err.Error())
				return
			}
			defer signedR.Close()
			fmt.Printf("%s | Downloaded signed deployment package. Took %s.\n", folder, t9())
			//
			fmt.Printf("%s | Hashing signed deployment package.\n", folder)
			t10 := newTimer()
			signedHash, err := hashR(signedR)
			if err != nil {
				fmt.Printf("%s | Failed to hash signed deployment package: %s\n", folder, err.Error())
				return
			}
			fmt.Printf("%s | Hashed signed deployment package: %s. Took %s.\n", folder, signedHash, t10())
			//
			fmt.Printf("%s | Copying signed deployment package to signed/.\n", folder)
			t11 := newTimer()
			err = copyObject(s3Client, bucket, stagingKey, signedKey, map[string]string{
				"unsignedHash":     unsignedHash,
				"signedHash":       signedHash,
				"source-code-hash": signedHash,
			})
			if err != nil {
				fmt.Printf("%s | Failed to copy signed deployment package: %s\n", folder, err.Error())
				return
			}
			fmt.Printf("%s | Copied signed deployment package to signed/. Took %s.\n", folder, t11())
			//
			if !noUpdateFunctions {
				fmt.Printf("%s | Updating Lambda function code.\n", folder)
				t12 := newTimer()
				err = updateFunctionCode(lambdaClient, folder, bucket, signedKey)
				if err != nil {
					fmt.Printf("%s | Failed to update Lambda function code: %s\n", folder, err.Error())
					return
				}
				fmt.Printf("%s | Updated Lambda function code. Took %s.\n", folder, t12())
				//
				fmt.Printf("%s | Waiting for function code to update.\n", folder)
				t13 := newTimer()
				err = waitForFunctionUpdate(functionUpdatedWaiter, folder)
				if err != nil {
					fmt.Printf("%s | Failed to wait for function code to update: %s\n", folder, err.Error())
					return
				}
				fmt.Printf("%s | Function code is updated. Took %s.\n", folder, t13())
				//
				fmt.Printf("%s | Publishing new version of Lambda function.\n", folder)
				t14 := newTimer()
				functionVersion, err := publishLambdaVersion(lambdaClient, folder, signedHash)
				if err != nil {
					fmt.Printf("%s | Failed to publish function version: %s\n", folder, err.Error())
					return
				}
				fmt.Printf("%s | Published new version of Lambda function: %s. Took %s.\n", folder, functionVersion, t14())
				//
				fmt.Printf("%s | Updating alias of Lambda function.\n", folder)
				t15 := newTimer()
				err = updateFunctionAlias(lambdaClient, folder, functionVersion)
				if err != nil {
					fmt.Printf("%s | Failed to update alias of Lambda function: %s\n", folder, err.Error())
					return
				}
				fmt.Printf("%s | Updated alias of Lambda function. Took %s.\n", folder, t15())
			}
		}(folder)
	}
	wg.Wait()

	// err = cleanBinaries(s3Client, bucket, unsignedPrefix, stagingPrefix)
	// if err != nil {
	// 	panic(err)
	// }

	fmt.Printf("\n")
	fmt.Printf("Took %s.\n", t0())
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

// func stopTimer(startTime time.Time) string {
// 	duration := time.Now().Sub(startTime)
// 	minutes := int(duration.Minutes())
// 	seconds := int(duration.Seconds()) % 60
// 	if minutes == 0 {
// 		return fmt.Sprintf("%d seconds", seconds)
// 	}
// 	return fmt.Sprintf("%d minutes and %d seconds", minutes, seconds)
// }

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

// func cleanBinaries(client *s3.Client, bucket, unsignedPrefix, stagingPrefix string) error {
// 	fmt.Printf("Cleaning %s/%s and %s/%s.\n", bucket, unsignedPrefix, bucket, stagingPrefix)
// 	//
// 	unsignedOutput, err := client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
// 		Bucket: &bucket,
// 		Prefix: &unsignedPrefix,
// 	})
// 	//
// 	stagingOutput, err := client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
// 		Bucket: &bucket,
// 		Prefix: &stagingPrefix,
// 	})
// 	//
// 	prefixes := make([]s3Types.ObjectIdentifier, 0, len(unsignedOutput.Contents)+len(stagingOutput.Contents))
// 	for _, content := range unsignedOutput.Contents {
// 		prefixes = append(prefixes, s3Types.ObjectIdentifier{Key: content.Key})
// 	}
// 	for _, content := range stagingOutput.Contents {
// 		prefixes = append(prefixes, s3Types.ObjectIdentifier{Key: content.Key})
// 	}
// 	if len(prefixes) == 0 {
// 		fmt.Printf("Nothing to delete.\n")
// 		return nil
// 	}
// 	//
// 	deleteOutput, err := client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
// 		Bucket: &bucket,
// 		Delete: &s3Types.Delete{
// 			Objects: prefixes,
// 		},
// 	})
// 	if err != nil {
// 		return err
// 	}
// 	for _, deleted := range deleteOutput.Deleted {
// 		fmt.Printf("Deleted %s.\n", *deleted.Key)
// 	}
// 	return nil
// }

// //
// //     fmt.Printf("%s | Linting folder.\n", folder)
// //     t1 := newTimer()
// //     err = lintFolder(folder)
// //     if err != nil {
// //         fmt.Printf("%s | Failed to lint folder: %s.\n", folder, err.Error())
// //         return
// //     }
// //     fmt.Printf("%s | Linted folder. Took %s.\n", folder, t1())
// //
// func lintFolder(dir string) error {
// 	cmd := exec.Command("golangci-lint", "run")
// 	cmd.Dir = dir
// 	cmd.Stdout = os.Stdout
// 	cmd.Stderr = os.Stderr
// 	return cmd.Run()
// }

func buildExecutable(executablePath, dir string, environ []string) error {
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", executablePath)
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// //
// //     fmt.Printf("%s | Compressing executable.\n", folder)
// //     t3 = newTimer()
// //     err = compressExecutable(executablePath)
// //     if err != nil {
// //         fmt.Printf("%s | Failed to compress executable: %s.\n", folder, err.Error())
// //         return
// //     }
// //     fmt.Printf("%s | Compressed executable. Took %s.\n", folder, t3())
// //
// func compressExecutable(executablePath string) error {
// 	cmd := exec.Command("upx", "-qqq", executablePath)
// 	cmd.Stdout = os.Stdout
// 	cmd.Stderr = os.Stderr
// 	return cmd.Run()
// }

func zipExecutable(path string) (io.Reader, error) {
	targetF := &bytes.Buffer{}
	targetW := zip.NewWriter(targetF)
	defer targetW.Close()
	// create entry
	entryW, err := targetW.Create("main")
	if err != nil {
		return nil, err
	}
	// copy file into entry
	sourceF, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer sourceF.Close()
	_, err = io.Copy(entryW, sourceF)
	if err != nil {
		return nil, err
	}
	return targetF, nil
}

func duplicateR(r io.Reader) (io.Reader, io.Reader) {
	r2 := &bytes.Buffer{}
	r1 := io.TeeReader(r, r2)
	return r1, r2
}

func lenR(r io.Reader) int {
	buf := &bytes.Buffer{}
	buf.ReadFrom(r)
	return buf.Len()
}

func hashR(r io.Reader) (string, error) {
	h := sha256.New()
	_, err := io.Copy(h, r)
	if err != nil {
		return "", err
	}
	return string(base64.StdEncoding.EncodeToString(h.Sum(nil))), nil
}

func isUpToDate(client *s3.Client, bucket, key, unsignedHash string) (string, bool) {
	output, err := client.HeadObject(context.TODO(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", false
	}
	previous, ok := output.Metadata["unsignedhash"]
	if !ok {
		return "", false
	}
	if unsignedHash != previous {
		return previous, false
	}
	return previous, true
}

func putObject(client *s3.Client, bucket, key string, reader io.Reader, metadata map[string]string) (string, error) {
	output, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		Body:     reader,
		Metadata: metadata,
	})
	if err != nil {
		return "", err
	}
	return *output.VersionId, nil
}

func startSigningJob(client *signer.Client, profileName, bucket, unsignedKey, version, stagingPrefix string) (string, error) {
	output, err := client.StartSigningJob(context.TODO(), &signer.StartSigningJobInput{
		ClientRequestToken: nil,
		ProfileName:        aws.String(profileName),
		Source: &signerTypes.Source{
			S3: &signerTypes.S3Source{
				BucketName: aws.String(bucket),
				Key:        aws.String(unsignedKey),
				Version:    aws.String(version),
			},
		},
		Destination: &signerTypes.Destination{
			S3: &signerTypes.S3Destination{
				BucketName: aws.String(bucket),
				Prefix:     aws.String(stagingPrefix + "/"),
			},
		},
	})
	if err != nil {
		return "", err
	}
	return *output.JobId, nil
}

func waitForSigningJob(waiter *signer.SuccessfulSigningJobWaiter, jobId string) error {
	return waiter.Wait(context.TODO(), &signer.DescribeSigningJobInput{
		JobId: aws.String(jobId),
	}, 30*time.Second)
}

func deleteObject(client *s3.Client, bucket, key string) error {
	// fmt.Printf("%s | Deleting object: %s/%s.\n", folder, bucket, key)
	_, err := client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// fmt.Printf("%s | Failed to update Lambda function code: %s\n", folder, err.Error())
		return err
	}
	// fmt.Printf("%s | Updated Lambda function code.\n", folder)
	return nil
}

func getObject(client *s3.Client, bucket, key string) (io.ReadCloser, error) {
	output, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

func copyObject(client *s3.Client, bucket, sourceKey, targetKey string, metadata map[string]string) error {
	_, err := client.CopyObject(context.TODO(), &s3.CopyObjectInput{
		CopySource:        aws.String(bucket + "/" + sourceKey),
		Bucket:            aws.String(bucket),
		Key:               aws.String(targetKey),
		Metadata:          metadata,
		MetadataDirective: s3Types.MetadataDirective("REPLACE"),
	})
	if err != nil {
		return err
	}
	return nil
}

func updateFunctionCode(client *lambda.Client, functionName, bucket, key string) error {
	_, err := client.UpdateFunctionCode(context.TODO(), &lambda.UpdateFunctionCodeInput{
		FunctionName: aws.String(functionName),
		S3Bucket:     aws.String(bucket),
		S3Key:        aws.String(key),
	})
	if err != nil {
		return err
	}
	return nil
}

func waitForFunctionUpdate(waiter *lambda.FunctionUpdatedV2Waiter, functionName string) error {
	return waiter.Wait(context.TODO(), &lambda.GetFunctionInput{
		FunctionName: aws.String(functionName),
	}, 30*time.Second)
}

func publishLambdaVersion(client *lambda.Client, functionName, hash string) (string, error) {
	output, err := client.PublishVersion(context.TODO(), &lambda.PublishVersionInput{
		FunctionName: aws.String(functionName),
		CodeSha256:   aws.String(hash),
	})
	if err != nil {
		return "", err
	}
	return *output.Version, nil
}

func updateFunctionAlias(client *lambda.Client, functionName, version string) error {
	_, err := client.UpdateAlias(context.TODO(), &lambda.UpdateAliasInput{
		FunctionName:    aws.String(functionName),
		Name:            aws.String("TEST"),
		FunctionVersion: aws.String(version),
	})
	if err != nil {
		return err
	}
	return nil
}
