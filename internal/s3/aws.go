package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// AWSOptions for [NewAWSClient].
type AWSOptions struct {
	// Bucket name. Required.
	Bucket string
	// Region, defaulting to "auto" when neither this nor the AWS_REGION environment variable is set.
	Region string
	// Endpoint URL. Empty uses the AWS_ENDPOINT_URL_S3 environment variable or the AWS default.
	Endpoint string
	// Tigris sends the X-Tigris-Consistent header on conditional operations and consistent reads.
	// Detected from the endpoint host when nil.
	Tigris *bool
	// PathStyle addresses the bucket in the URL path instead of the host name, which stores
	// reached by a plain host name, such as a container, need.
	PathStyle bool
}

// AWSClient implements [Client] with aws-sdk-go-v2.
type AWSClient struct {
	client *awss3.Client
	bucket string
	tigris bool
}

// NewAWSClient for the given bucket, using the default AWS credential chain.
func NewAWSClient(ctx context.Context, opts AWSOptions) (*AWSClient, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}

	var loadOpts []func(*config.LoadOptions) error
	if opts.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(opts.Region))
	} else {
		loadOpts = append(loadOpts, config.WithDefaultRegion("auto"))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: loading aws config: %w", err)
	}

	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.PathStyle
		// S3-compatible stores do not all support the SDK's default checksums.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	tigris := false
	if opts.Tigris != nil {
		tigris = *opts.Tigris
	} else {
		endpoint := opts.Endpoint
		if endpoint == "" {
			endpoint = aws.ToString(cfg.BaseEndpoint)
		}
		tigris = isTigrisEndpoint(endpoint)
	}

	return &AWSClient{client: client, bucket: opts.Bucket, tigris: tigris}, nil
}

func isTigrisEndpoint(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return strings.Contains(endpoint, "tigris")
	}
	return strings.Contains(u.Host, "tigris") || strings.HasSuffix(u.Host, "storage.dev")
}

// Tigris reports whether the client sends Tigris consistency headers.
func (c *AWSClient) Tigris() bool {
	return c.tigris
}

func (c *AWSClient) consistent() func(*awss3.Options) {
	return func(o *awss3.Options) {
		if c.tigris {
			o.APIOptions = append(o.APIOptions, smithyhttp.AddHeaderValue("X-Tigris-Consistent", "true"))
		}
	}
}

func (c *AWSClient) Get(ctx context.Context, key string, opts GetOptions) (Object, error) {
	input := &awss3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	if opts.IfNoneMatch != "" {
		input.IfNoneMatch = aws.String(opts.IfNoneMatch)
	}
	if opts.Range != nil {
		input.Range = aws.String(fmt.Sprintf("bytes=%d-%d", opts.Range.Offset, opts.Range.Offset+opts.Range.Length-1))
	}
	var callOpts []func(*awss3.Options)
	if opts.Consistent || opts.IfNoneMatch != "" {
		callOpts = append(callOpts, c.consistent())
	}
	res, err := c.client.GetObject(ctx, input, callOpts...)
	if err != nil {
		return Object{}, mapError(err)
	}
	defer func() {
		_ = res.Body.Close()
	}()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return Object{}, fmt.Errorf("s3: reading body of %q: %w", key, err)
	}
	return Object{Body: body, ETag: aws.ToString(res.ETag)}, nil
}

func (c *AWSClient) Put(ctx context.Context, key string, body []byte, opts PutOptions) (string, error) {
	input := &awss3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}
	if opts.IfNoneMatch {
		input.IfNoneMatch = aws.String("*")
	}
	if opts.IfMatch != "" {
		input.IfMatch = aws.String(opts.IfMatch)
	}
	var callOpts []func(*awss3.Options)
	if opts.Conditional() {
		callOpts = append(callOpts, c.consistent())
	}
	res, err := c.client.PutObject(ctx, input, callOpts...)
	if err != nil {
		return "", mapError(err)
	}
	return aws.ToString(res.ETag), nil
}

func (c *AWSClient) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		err = mapError(err)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (c *AWSClient) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var infos []ObjectInfo
	paginator := awss3.NewListObjectsV2Paginator(c.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, o := range page.Contents {
			infos = append(infos, ObjectInfo{
				Key:          aws.ToString(o.Key),
				Size:         aws.ToInt64(o.Size),
				LastModified: aws.ToTime(o.LastModified),
			})
		}
	}
	return infos, nil
}

func mapError(err error) error {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		switch re.HTTPStatusCode() {
		case 304:
			return ErrNotModified
		case 404:
			return ErrNotFound
		case 412:
			return ErrPreconditionFailed
		}
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotModified":
			return ErrNotModified
		case "NoSuchKey", "NotFound":
			return ErrNotFound
		case "PreconditionFailed":
			return ErrPreconditionFailed
		}
	}
	return fmt.Errorf("s3: %w", err)
}
