package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/signer"
	signerTypes "github.com/aws/aws-sdk-go-v2/service/signer/types"
)

type data struct {
	// context to use in api calls
	ctx context.Context
	// flags
	noUpdateFunctions bool
	force             bool
	// environment variables to pass to go build
	environ []string
	// s3 config
	s3             *s3.Client
	bucket         string
	unsignedPrefix string
	stagingPrefix  string
	signedPrefix   string
	// signer config
	signer           *signer.Client
	signingProfile   string
	signingJobWaiter *signer.SuccessfulSigningJobWaiter
	// lambda config
	lambda                *lambda.Client
	functionUpdatedWaiter *lambda.FunctionUpdatedV2Waiter
}

func (d *data) run(folder string) error {
	executablePath := fmt.Sprintf("/tmp/%s", folder)
	unsignedKey := fmt.Sprintf("%s/%s.zip", d.unsignedPrefix, folder)
	signedKey := fmt.Sprintf("%s/%s.zip", d.signedPrefix, folder)
	//
	unsignedHash, err := d.hashSourceCode(folder)
	if err != nil {
		return err
	}
	if !d.force {
		isUpToDate, err := d.isUpToDate(folder, signedKey, unsignedHash)
		if err != nil {
			return err
		}
		if isUpToDate {
			return nil
		}
	}
	err = d.buildExecutable(folder, executablePath)
	if err != nil {
		return err
	}
	defer d.deleteFile(folder, executablePath)
	unsignedR, err := d.zipExecutable(folder, executablePath)
	if err != nil {
		return err
	}
	unsignedR1, err := d.sizeExecutable(folder, unsignedR)
	if err != nil {
		return err
	}
	objectVersion, err := d.putObject(folder, unsignedKey, unsignedR1)
	if err != nil {
		return err
	}
	defer d.deleteObject(folder, unsignedKey)
	jobId, err := d.startSigningJob(folder, unsignedKey, objectVersion)
	if err != nil {
		return err
	}
	stagingKey := d.stagingPrefix + "/" + jobId + ".zip"
	err = d.waitForSigningJob(folder, jobId)
	if err != nil {
		return err
	}
	defer d.deleteObject(folder, stagingKey)
	signedR, err := d.getObject(folder, stagingKey)
	if err != nil {
		return err
	}
	defer signedR.Close()
	signedHash, err := d.hashObject(folder, signedR)
	if err != nil {
		return err
	}
	err = d.copyObject(folder, stagingKey, signedKey, map[string]string{
		"unsignedHash":     unsignedHash,
		"signedHash":       signedHash,
		"source-code-hash": signedHash,
	})
	if err != nil {
		return err
	}
	if d.noUpdateFunctions {
		return nil
	}
	err = d.updateFunctionCode(folder, signedKey)
	if err != nil {
		return err
	}
	err = d.waitForFunctionUpdate(folder)
	if err != nil {
		return err
	}
	functionVersion, err := d.publishLambdaVersion(folder, signedHash)
	if err != nil {
		return err
	}
	err = d.updateFunctionAlias(folder, functionVersion)
	if err != nil {
		return err
	}
	return nil
}

func (d *data) hashSourceCode(folder string) (string, error) {
	fmt.Printf("%s | Hashing source code.\n", folder)
	// search for files that match the patterns go.* or *.go e.g. go.mod go.sum main.go
	filenames := []string{}
	a, err := filepath.Glob(folder + "/go.*")
	if err != nil {
		fmt.Printf("%s | Failed to search with go.*: %s.\n", folder, err.Error())
		return "", err
	}
	filenames = append(filenames, a...)
	b, err := filepath.Glob(folder + "/*.go")
	if err != nil {
		fmt.Printf("%s | Failed to search with *.go: %s.\n", folder, err.Error())
		return "", err
	}
	filenames = append(filenames, b...)
	sort.Strings(filenames)
	fmt.Printf(
		"%s | Hashing %d files: %s\n",
		folder,
		len(filenames),
		strings.Join(filenames, ", "),
	)
	// hash files
	h := sha256.New()
	for _, filename := range filenames {
		file, err := os.Open(filename)
		if err != nil {
			fmt.Printf("%s | Failed to open file (%s): %s.\n", folder, filename, err.Error())
			return "", err
		}
		_, err = io.Copy(h, file)
		if err != nil {
			fmt.Printf("%s | Failed to hash file (%s): %s.\n", folder, filename, err.Error())
			return "", err
		}
	}
	hash := string(base64.StdEncoding.EncodeToString(h.Sum(nil)))
	fmt.Printf("%s | Hashed source code: %s\n", folder, hash)
	return hash, nil
}

func (d *data) deleteFile(folder, path string) error {
	fmt.Printf("%s | Deleting file: %s.\n", folder, path)
	err := os.Remove(path)
	if err != nil {
		fmt.Printf("%s | Failed to delete file (%s): %s.\n", folder, path, err.Error())
		return err
	}
	fmt.Printf("%s | Deleted file: %s.\n", folder, path)
	return nil
}

func (d *data) buildExecutable(folder, executablePath string) error {
	fmt.Printf("%s | Building executable.\n", folder)
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", executablePath)
	cmd.Dir = folder
	cmd.Env = d.environ
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		fmt.Printf("%s | Failed to build executable: %s.\n", folder, err.Error())
		return err
	}
	fmt.Printf("%s | Built executable.\n", folder)
	return nil
}

func (d *data) zipExecutable(folder, executablePath string) (io.Reader, error) {
	fmt.Printf("%s | Zipping executable.\n", folder)
	targetF := &bytes.Buffer{}
	targetW := zip.NewWriter(targetF)
	defer targetW.Close()
	// create entry
	entryW, err := targetW.Create("main")
	if err != nil {
		fmt.Printf("%s | Failed to zip executable: %s.\n", folder, err.Error())
		return nil, err
	}
	// copy file into entry
	sourceF, err := os.Open(executablePath)
	if err != nil {
		fmt.Printf("%s | Failed to zip executable: %s.\n", folder, err.Error())
		return nil, err
	}
	defer sourceF.Close()
	_, err = io.Copy(entryW, sourceF)
	if err != nil {
		fmt.Printf("%s | Failed to zip executable: %s.\n", folder, err.Error())
		return nil, err
	}
	fmt.Printf("%s | Zipped executable.\n", folder)
	return targetF, nil
}

func (d *data) sizeExecutable(folder string, r io.Reader) (io.Reader, error) {
	fmt.Printf("%s | Getting size of unsigned deployment package.\n", folder)
	// create a buffer to return back to the caller
	copyBuf := &bytes.Buffer{}
	// create a buffer to calculate the length of the input
	lenBuf := &bytes.Buffer{}
	// copy data from the input reader into the copy buffer
	_, err := lenBuf.ReadFrom(io.TeeReader(r, copyBuf))
	if err != nil {
		fmt.Printf(
			"%s | Failed to get size of unsigned deployment package: %s.\n",
			folder,
			err.Error(),
		)
		return nil, err
	}
	// convert size to megabytes
	size := float64(lenBuf.Len()) / 1000000
	fmt.Printf("%s | Size of unsigned deployment package: %.2f M.\n", folder, size)
	// return the copy buffer so the data can still be accessed
	return copyBuf, nil
}

// Returns true if previous deployment package is up to date.
// Returns false if the previous deployment package does not exist.
// Returns false if the previous deployment package does not have metadata.
// Returns false if the previous deployment package does not have "unsignedhash".
// Returns false if the previous deployment package's "unsignedhash" is not unsignedHash.
// Returns false if the API call failed.
// TODO(kesav): Return false if the API failed with a 404 error.
// TODO(kesav): Return an error if the API call failed with any other error.
func (d *data) isUpToDate(folder, signedKey string, unsignedHash string) (bool, error) {
	fmt.Printf("%s | Checking if previous deployment package is up to date.\n", folder)
	output, err := d.s3.HeadObject(d.ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(signedKey),
	})
	if err != nil {
		fmt.Printf(
			"%s | Failed to get previous deployment package %s, proceeding: %s.\n",
			folder,
			signedKey,
			err.Error(),
		)
		return false, nil
	}
	if output.Metadata == nil {
		fmt.Printf(
			"%s | Previous deployment package does not have metadata, proceeding.\n",
			folder,
		)
		return false, nil
	}
	previous, ok := output.Metadata["unsignedhash"]
	if !ok {
		fmt.Printf(
			"%s | Previous deployment package does not have unsignedhash, proceeding.\n",
			folder,
		)
		return false, nil
	}
	if unsignedHash != previous {
		fmt.Printf("%s | Previous deployment is out of date, proceeding: %s.\n", folder, previous)
		return false, nil
	}
	fmt.Printf("%s | Deployment package is up to date, stopping.\n", folder)
	return true, nil
}

func (d *data) putObject(folder, unsignedKey string, reader io.Reader) (string, error) {
	fmt.Printf("%s | Uploading unsigned deployment package to S3.\n", folder)
	output, err := d.s3.PutObject(d.ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(unsignedKey),
		Body:   reader,
	})
	if err != nil {
		fmt.Printf("%s | Failed to upload unsigned deployment package: %s\n", folder, err.Error())
		return "", err
	}
	fmt.Printf(
		"%s | Pushed unsigned deployment package to S3 with version ID: %s.\n",
		folder,
		*output.VersionId, // what if versioning is not enabled on the bucket?
	)
	return *output.VersionId, nil
}

func (d *data) startSigningJob(folder, unsignedKey, version string) (string, error) {
	fmt.Printf("%s | Starting signing job.\n", folder)
	output, err := d.signer.StartSigningJob(d.ctx, &signer.StartSigningJobInput{
		ClientRequestToken: nil,
		ProfileName:        aws.String(d.signingProfile),
		Source: &signerTypes.Source{
			S3: &signerTypes.S3Source{
				BucketName: aws.String(d.bucket),
				Key:        aws.String(unsignedKey),
				Version:    aws.String(version),
			},
		},
		Destination: &signerTypes.Destination{
			S3: &signerTypes.S3Destination{
				BucketName: aws.String(d.bucket),
				Prefix:     aws.String(d.stagingPrefix + "/"),
			},
		},
	})
	if err != nil {
		fmt.Printf("%s | Failed to start signing job: %s\n", folder, err.Error())
		return "", err
	}
	fmt.Printf("%s | Started signing job with id: %s.\n", folder, *output.JobId)
	return *output.JobId, nil
}

func (d *data) waitForSigningJob(folder string, jobId string) error {
	fmt.Printf("%s | Waiting for signing job to complete.\n", folder)
	err := d.signingJobWaiter.Wait(d.ctx, &signer.DescribeSigningJobInput{
		JobId: aws.String(jobId),
	}, 30*time.Second)
	if err != nil {
		fmt.Printf("%s | Failed to wait for signing job to complete: %s\n", folder, err.Error())
		return err
	}
	fmt.Printf("%s | Signing job is complete.\n", folder)
	return nil
}

func (d *data) deleteObject(folder, key string) error {
	fmt.Printf("%s | Deleting object: %s.\n", folder, key)
	_, err := d.s3.DeleteObject(d.ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		fmt.Printf("%s | Failed to delete object (%s): %s\n", folder, key, err.Error())
		return err
	}
	fmt.Printf("%s | Deleted object: %s.\n", folder, key)
	return nil
}

func (d *data) getObject(folder string, key string) (io.ReadCloser, error) {
	fmt.Printf("%s | Downloading signed deployment package.\n", folder)
	output, err := d.s3.GetObject(d.ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		fmt.Printf("%s | Failed to download signed deployment package: %s\n", folder, err.Error())
		return nil, err
	}
	fmt.Printf("%s | Downloaded signed deployment package.\n", folder)
	return output.Body, nil
}

func (d *data) hashObject(folder string, r io.Reader) (string, error) {
	fmt.Printf("%s | Hashing signed deployment package.\n", folder)
	h := sha256.New()
	_, err := io.Copy(h, r)
	if err != nil {
		fmt.Printf("%s | Failed to hash signed deployment package: %s.\n", folder, err.Error())
		return "", err
	}
	hash := string(base64.StdEncoding.EncodeToString(h.Sum(nil)))
	fmt.Printf("%s | Hashed signed deployment package: %s.\n", folder, hash)
	return hash, nil
}

func (d *data) copyObject(folder, stagingKey, signedKey string, metadata map[string]string) error {
	fmt.Printf("%s | Copying signed deployment package to signed/.\n", folder)
	_, err := d.s3.CopyObject(d.ctx, &s3.CopyObjectInput{
		CopySource:        aws.String(d.bucket + "/" + stagingKey),
		Bucket:            aws.String(d.bucket),
		Key:               aws.String(signedKey),
		Metadata:          metadata,
		MetadataDirective: s3Types.MetadataDirective("REPLACE"),
	})
	if err != nil {
		fmt.Printf("%s | Failed to copy signed deployment package: %s\n", folder, err.Error())
		return err
	}
	fmt.Printf("%s | Copied signed deployment package to signed/.\n", folder)
	return nil
}

func (d *data) updateFunctionCode(folder, signedKey string) error {
	fmt.Printf("%s | Updating Lambda function code.\n", folder)
	_, err := d.lambda.UpdateFunctionCode(d.ctx, &lambda.UpdateFunctionCodeInput{
		FunctionName: aws.String(folder),
		S3Bucket:     aws.String(d.bucket),
		S3Key:        aws.String(signedKey),
	})
	if err != nil {
		fmt.Printf("%s | Failed to update Lambda function code: %s\n", folder, err.Error())
		return err
	}
	fmt.Printf("%s | Updated Lambda function code.\n", folder)
	return nil
}

func (d *data) waitForFunctionUpdate(folder string) error {
	fmt.Printf("%s | Waiting for function code to update.\n", folder)
	err := d.functionUpdatedWaiter.Wait(d.ctx, &lambda.GetFunctionInput{
		FunctionName: aws.String(folder),
	}, 30*time.Second)
	if err != nil {
		fmt.Printf("%s | Failed to wait for function code to update: %s\n", folder, err.Error())
		return err
	}
	fmt.Printf("%s | Function code is updated.\n", folder)
	return nil
}

func (d *data) publishLambdaVersion(folder, hash string) (string, error) {
	fmt.Printf("%s | Publishing new version of Lambda function.\n", folder)
	output, err := d.lambda.PublishVersion(d.ctx, &lambda.PublishVersionInput{
		FunctionName: aws.String(folder),
		CodeSha256:   aws.String(hash),
	})
	if err != nil {
		fmt.Printf("%s | Failed to publish function version: %s\n", folder, err.Error())
		return "", err
	}
	fmt.Printf("%s | Published new version of Lambda function: %s.\n", folder, *output.Version)
	return *output.Version, nil
}

func (d *data) updateFunctionAlias(folder, version string) error {
	fmt.Printf("%s | Updating alias of Lambda function.\n", folder)
	_, err := d.lambda.UpdateAlias(d.ctx, &lambda.UpdateAliasInput{
		FunctionName:    aws.String(folder),
		Name:            aws.String("TEST"),
		FunctionVersion: aws.String(version),
	})
	if err != nil {
		fmt.Printf("%s | Failed to update alias of Lambda function: %s\n", folder, err.Error())
		return err
	}
	fmt.Printf("%s | Updated alias of Lambda function.\n", folder)
	return nil
}
