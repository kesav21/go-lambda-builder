
# Go Lambda Builder

This project is intended to help deploy a multitude (100+) of Go Lambda functions in parallell using goroutines.

Some notable features include:
- Each function is deployed in parallel with all the other functions.
- Each function is given a source code hash, which is compared at deployment time to
  ensure that a function is only deployed if there is a change to its source code.
- Each executable is signed by AWS Signer for additional security. This can be turned off.
- Each new deployment package is automatically uploaded to S3 and each function is
  automatically updated with the latest deployment package.

