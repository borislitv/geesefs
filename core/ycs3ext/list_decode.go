package ycs3ext

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/private/protocol"
	"github.com/aws/aws-sdk-go/service/s3"
)

// Decode the ext-v1 schema directly from the response stream. The SDK's generic
// decoder first retains an XMLNode/map tree for the entire page, then reflects
// it into these same fields. No token byte slices or decompressor buffers escape
// this decoder; returned strings own their data.
func unmarshalListExt(r *request.Request) {
	defer r.HTTPResponse.Body.Close()
	var out ListObjectsV1ExtOutput
	d := xml.NewDecoder(r.HTTPResponse.Body)
	err := d.Decode(&out)
	if err == nil {
		// Reading through EOF both validates trailing XML and checks a compressed
		// response's trailer/checksum. Closing a gzip reader alone does neither.
		for {
			var token xml.Token
			token, err = d.Token()
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				break
			}
			if text, ok := token.(xml.CharData); ok && strings.TrimSpace(string(text)) == "" {
				continue
			}
			switch token.(type) {
			case xml.Comment, xml.ProcInst:
				continue
			}
			err = fmt.Errorf("unexpected XML after ListBucketResult")
			break
		}
	}
	if err != nil {
		r.Error = awserr.NewRequestFailure(
			awserr.New(request.ErrCodeSerialization, "failed to decode ext-v1 listing", err),
			r.HTTPResponse.StatusCode, r.RequestID)
		return
	}
	*r.Data.(*ListObjectsV1ExtOutput) = out
}

// UnmarshalXML implements the ext-v1 response schema without a generic object
// graph. Unknown elements are skipped as subtrees for forward compatibility.
func (out *ListObjectsV1ExtOutput) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "ListBucketResult" {
		return fmt.Errorf("expected ListBucketResult, got %s", start.Name.Local)
	}
	return listElements(d, func(child xml.StartElement) error {
		var field interface{}
		switch child.Name.Local {
		case "Contents":
			item := &ListObjectExt{}
			if err := decodeListObject(d, item); err != nil {
				return err
			}
			out.Contents = append(out.Contents, item)
			return nil
		case "CommonPrefixes":
			prefix := &s3.CommonPrefix{}
			err := listElements(d, func(entry xml.StartElement) error {
				if entry.Name.Local == "Prefix" {
					return listScalar(d, &prefix.Prefix)
				}
				return d.Skip()
			})
			out.CommonPrefixes = append(out.CommonPrefixes, prefix)
			return err
		case "ContinuationToken":
			field = &out.ContinuationToken
		case "Delimiter":
			field = &out.Delimiter
		case "EncodingType":
			field = &out.EncodingType
		case "IsTruncated":
			field = &out.IsTruncated
		case "KeyCount":
			field = &out.KeyCount
		case "MaxKeys":
			field = &out.MaxKeys
		case "Name":
			field = &out.Name
		case "NextContinuationToken":
			field = &out.NextContinuationToken
		case "Prefix":
			field = &out.Prefix
		case "StartAfter":
			field = &out.StartAfter
		default:
			return d.Skip()
		}
		return listScalar(d, field)
	})
}

func decodeListObject(d *xml.Decoder, item *ListObjectExt) error {
	return listElements(d, func(child xml.StartElement) error {
		var field interface{}
		switch child.Name.Local {
		case "Key":
			field = &item.Key
		case "ETag":
			field = &item.ETag
		case "LastModified":
			field = &item.LastModified
		case "Size":
			field = &item.Size
		case "StorageClass":
			field = &item.StorageClass
		case "Owner":
			item.Owner = &s3.Owner{}
			return listElements(d, func(entry xml.StartElement) error {
				switch entry.Name.Local {
				case "ID":
					return listScalar(d, &item.Owner.ID)
				case "DisplayName":
					return listScalar(d, &item.Owner.DisplayName)
				default:
					return d.Skip()
				}
			})
		case "Metadata":
			var name, value *string
			err := listElements(d, func(entry xml.StartElement) error {
				switch entry.Name.Local {
				case "Name":
					return listScalar(d, &name)
				case "Value":
					return listScalar(d, &value)
				default:
					return d.Skip()
				}
			})
			if err == nil && name != nil {
				if value == nil {
					return fmt.Errorf("metadata %q has no Value", *name)
				}
				if item.UserMetadata == nil {
					item.UserMetadata = make(map[string]*string)
				}
				item.UserMetadata[*name] = value
			}
			return err
		default:
			return d.Skip()
		}
		return listScalar(d, field)
	})
}

// The start token has already been consumed. encoding/xml validates matching
// end tags and reports truncated input, including in skipped unknown subtrees.
func listElements(d *xml.Decoder, visit func(xml.StartElement) error) error {
	for {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch token := token.(type) {
		case xml.StartElement:
			if err := visit(token); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

func listScalar(d *xml.Decoder, field interface{}) error {
	var text strings.Builder
	for {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch token := token.(type) {
		case xml.CharData:
			text.Write(token)
		case xml.StartElement:
			return fmt.Errorf("unexpected element %s in listing scalar", token.Name.Local)
		case xml.EndElement:
			value := text.String()
			switch field := field.(type) {
			case **string:
				*field = &value
			case **int64:
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return err
				}
				*field = &n
			case **bool:
				b, err := strconv.ParseBool(value)
				if err != nil {
					return err
				}
				*field = &b
			case **time.Time:
				t, err := protocol.ParseTime(protocol.ISO8601TimeFormatName, value)
				if err != nil {
					return err
				}
				*field = &t
			}
			return nil
		}
	}
}
