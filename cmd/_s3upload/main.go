// One-off S3 upload helper. Lives under cmd/_s3upload/ — the leading
// underscore makes `go build ./...` and `go test ./...` skip this
// directory, so it doesn't interfere with the regular build. Run with:
//
//	go run ./cmd/_s3upload list
//	go run ./cmd/_s3upload upload <bucket> <object> <content>
//	go run ./cmd/_s3upload ls <bucket>
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	endpoint  = "192.168.0.107:9000"
	accessKey = "nimbus_85af3ed277e8d80e87cd18f0fda1699d"
	secretKey = "a8c650593991105e33e7bcd1e65cba678e2369ddb7c024bda26ddb272d9188f4"
)

func main() {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatalf("minio.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if len(os.Args) < 2 {
		log.Fatalf("usage: %s <list|upload|ls>", os.Args[0])
	}

	switch os.Args[1] {
	case "list":
		bs, err := mc.ListBuckets(ctx)
		if err != nil {
			log.Fatalf("ListBuckets: %v", err)
		}
		if len(bs) == 0 {
			fmt.Println("(no buckets)")
			return
		}
		for _, b := range bs {
			fmt.Printf("%s\t%s\n", b.Name, b.CreationDate.Format(time.RFC3339))
		}
	case "upload":
		if len(os.Args) < 5 {
			log.Fatalf("usage: %s upload <bucket> <object> <content>", os.Args[0])
		}
		bucket, obj, content := os.Args[2], os.Args[3], os.Args[4]
		info, err := mc.PutObject(ctx, bucket, obj,
			strings.NewReader(content), int64(len(content)),
			minio.PutObjectOptions{ContentType: "text/plain"})
		if err != nil {
			log.Fatalf("PutObject: %v", err)
		}
		fmt.Printf("uploaded %s/%s  (%d bytes, etag %s)\n", info.Bucket, info.Key, info.Size, info.ETag)
	case "ls":
		if len(os.Args) < 3 {
			log.Fatalf("usage: %s ls <bucket>", os.Args[0])
		}
		bucket := os.Args[2]
		for o := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if o.Err != nil {
				log.Fatalf("ListObjects: %v", o.Err)
			}
			fmt.Printf("%-50s %8d  %s\n", o.Key, o.Size, o.LastModified.Format(time.RFC3339))
		}
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}
