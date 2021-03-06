package dynamodb

/*
 * Copyright 2020 Aldelo, LP
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// =================================================================================================================
// AWS CREDENTIAL:
//		use $> aws configure (to set aws access key and secret to target machine)
//		Store AWS Access ID and Secret Key into Default Profile Using '$ aws configure' cli
// =================================================================================================================

import (
	awshttp2 "github.com/aldelo/common/wrapper/aws"
	"github.com/aldelo/common/wrapper/aws/awsregion"
	"github.com/aws/aws-dax-go/dax"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/dynamodb/expression"
	"net/http"

	util "github.com/aldelo/common"
	"context"
	"errors"
	"time"
)

// ================================================================================================================
// STRUCTS
// ================================================================================================================

// DynamoDB struct encapsulates the AWS DynamoDB access functionality
//
// Notes:
//		1) to use dax, must be within vpc with dax cluster subnet pointing to private ip subnet of the vpc
//		2) dax is not accessible outside of vpc
// 		3) on ec2 or container within vpc, also need aws credential via aws cli too = aws configure
type DynamoDB struct {
	// define the AWS region that dynamodb is serviced from
	AwsRegion awsregion.AWSRegion

	// custom http2 client options
	HttpOptions *awshttp2.HttpClientSettings

	// define the Dax Endpoint (required if using DAX)
	DaxEndpoint string

	// dynamodb connection object
	cn *dynamodb.DynamoDB

	// dax connection object
	cnDax *dax.Dax

	// if dax is enabled, skip dax will skip dax and route direct to DynamoDB
	// if dax is not enabled, skip dax true or not will always route to DynamoDB
	SkipDax bool

	// operating table
	TableName string
	PKName string
	SKName string

	// last execute param string
	LastExecuteParamsPayload string
}

// DynamoDBError struct contains special status info including error and retry advise
type DynamoDBError struct {
	ErrorMessage string
	SuppressError bool

	AllowRetry bool
	RetryNeedsBackOff bool
}

// Error returns error string of the struct object
func (e *DynamoDBError) Error() string {
	return e.ErrorMessage
}

// DynamoDBTableKeys struct defines the PK and SK fields to be used in key search (Always PK and SK)
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// ResultItemPtr = optional, used with TransactionGetItems() to denote output unmarshal object target
type DynamoDBTableKeys struct {
	PK string
	SK string

	ResultItemPtr interface{}		`dynamodbav:"-"`

	resultProcessed bool
}

// DynamoDBUnprocessedItemsAndKeys defines struct to slices of items and keys
type DynamoDBUnprocessedItemsAndKeys struct {
	PutItems []map[string]*dynamodb.AttributeValue
	DeleteKeys []*DynamoDBTableKeys
}

// UnmarshalPutItems will convert struct's PutItems into target slice of struct objects
//
// notes:
//		resultItemsPtr interface{} = Input is Slice of Actual Struct Objects
func (u *DynamoDBUnprocessedItemsAndKeys) UnmarshalPutItems(resultItemsPtr interface{}) error {
	if u == nil {
		return errors.New("UnmarshalPutItems Failed: (Validate) " + "DynamoDBUnprocessedItemsAndKeys Object Nil")
	}

	if resultItemsPtr == nil {
		return errors.New("UnmarshalPutItems Failed: (Validate) " + "ResultItems Object Nil")
	}

	if err := dynamodbattribute.UnmarshalListOfMaps(u.PutItems, resultItemsPtr); err != nil {
		return errors.New("UnmarshalPutItems Failed: (Unmarshal) " + err.Error())
	} else {
		// success
		return nil
	}
}

// DynamoDBUpdateItemInput defines a single item update instruction
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//		pkValue = required, value of partition key to seek
//		skValue = optional, value of sort key to seek; set to blank if value not provided
//
//		updateExpression = required, ATTRIBUTES ARE CASE SENSITIVE; set remove add or delete action expression, see Rules URL for full detail
//			Rules:
//				1) https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Expressions.UpdateExpressions.html
//			Usage Syntax:
//				1) Action Keywords are: set, add, remove, delete
//				2) Each Action Keyword May Appear in UpdateExpression Only Once
//				3) Each Action Keyword Grouping May Contain One or More Actions, Such as 'set price=:p, age=:age, etc' (each action separated by comma)
//				4) Each Action Keyword Always Begin with Action Keyword itself, such as 'set ...', 'add ...', etc
//				5) If Attribute is Numeric, Action Can Perform + or - Operation in Expression, such as 'set age=age-:newAge, price=price+:price, etc'
//				6) If Attribute is Slice, Action Can Perform Slice Element Operation in Expression, such as 'set age[2]=:newData, etc'
//				7) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//					a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//				8) When Attribute is a List, Use list_append(a, b, ...) in Expression to append elements (list_append() is case sensitive)
//					a) set #ri = list_append(#ri, :vals) where :vals represents one or more of elements to add as in L
//				9) if_not_exists(path, value)
//					a) Avoids existing attribute if already exists
//					b) set price = if_not_exists(price, :p)
//					c) if_not_exists is case sensitive; path is the existing attribute to check
//				10) Action Type Purposes
//					a) SET = add one or more attributes to an item; overrides existing attributes in item with new values; if attribute is number, able to perform + or - operations
//					b) REMOVE = remove one or more attributes from an item, to remove multiple attributes, separate by comma; remove element from list use xyz[1] index notation
//					c) ADD = adds a new attribute and its values to an item; if attribute is number and already exists, value will add up or subtract
//					d) DELETE = supports only on set data types; deletes one or more elements from a set, such as 'delete color :c'
//				11) Example
//					a) set age=:age, name=:name, etc
//					b) set age=age-:age, num=num+:num, etc
//
//		conditionExpress = optional, ATTRIBUTES ARE CASE SENSITIVE; sets conditions for this condition expression, set to blank if not used
//				Usage Syntax:
//					1) "size(info.actors) >= :num"
//						a) When Length of Actors Attribute Value is Equal or Greater Than :num, ONLY THEN UpdateExpression is Performed
//					2) ExpressionAttributeName and ExpressionAttributeValue is Still Defined within ExpressionAttributeNames and ExpressionAttributeValues Where Applicable
//
//		expressionAttributeNames = optional, ATTRIBUTES ARE CASE SENSITIVE; set nil if not used, must define for attribute names that are reserved keywords such as year, data etc. using #xyz
//			Usage Syntax:
//				1) map[string]*string: where string is the #xyz, and *string is the original xyz attribute name
//					a) map[string]*string { "#xyz": aws.String("Xyz"), }
//				2) Add to Map
//					a) m := make(map[string]*string)
//					b) m["#xyz"] = aws.String("Xyz")
//
//		expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//			Usage Syntax:
//				1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//					a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//				2) Add to Map
//					a) m := make(map[string]*dynamodb.AttributeValue)
//					b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//				3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//					a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//					b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
type DynamoDBUpdateItemInput struct {
	PK string
	SK string
	UpdateExpression string
	ConditionExpression string
	ExpressionAttributeNames map[string]*string
	ExpressionAttributeValues map[string]*dynamodb.AttributeValue
}

// DynamoDBTransactionWrites defines one or more items to put, update or delete
//
// notes
//		PutItems interface{} = is Slice of PutItems: []Xyz
//			a) We use interface{} because []interface{} will require each element conversion (instead we will handle conversion by internal code)
//			b) PutItems ALWAYS Slice of Struct (Value), NOT pointers to Structs
type DynamoDBTransactionWrites struct {
	PutItems interface{}
	UpdateItems []*DynamoDBUpdateItemInput
	DeleteItems []*DynamoDBTableKeys
	TableNameOverride string
}

// MarshalPutItems will marshal dynamodb transaction writes' put items into []map[string]*dynamodb.AttributeValue
func (w *DynamoDBTransactionWrites) MarshalPutItems() (result []map[string]*dynamodb.AttributeValue, err error) {
	if w == nil {
		return nil, errors.New("MarshalPutItems Failed: (Validate) " + "DynamoDBTransactionWrites Object Nil")
	}

	// validate
	if w.PutItems == nil {
		// no PutItems
		return nil, nil
	}

	// get []interface{}
	itemsIf := util.SliceObjectsToSliceInterface(w.PutItems)

	if itemsIf == nil {
		// no PutItems
		return nil, errors.New("MarshalPutItems Failed: (Slice Convert) " + "Interface Slice Nil")
	}

	if len(itemsIf) <= 0 {
		// no PutItems
		return nil, nil
	}

	// loop thru each put item to marshal
	for _, v := range itemsIf {
		if m, e := dynamodbattribute.MarshalMap(v); e != nil {
			return nil, errors.New("MarshalPutItems Failed: (Marshal) " + e.Error())
		} else {
			if m != nil {
				result = append(result, m)
			} else {
				return nil, errors.New("MarshalPutItems Failed: (Marshal) " + "Marshaled Result Nil")
			}
		}
	}

	// return result
	return result, nil
}

// DynamoDBTransactionReads defines one or more items to get by PK / SK
type DynamoDBTransactionReads struct {
	Keys []*DynamoDBTableKeys
	TableNameOverride string
}

// ================================================================================================================
// STRUCTS FUNCTIONS
// ================================================================================================================

// ----------------------------------------------------------------------------------------------------------------
// utility functions
// ----------------------------------------------------------------------------------------------------------------

// handleError is an internal helper method to evaluate dynamodb error,
// and to advise if retry, immediate retry, suppress error etc error handling advisory
//
// notes:
//		RetryNeedsBackOff = true indicates when doing retry, must wait an arbitrary time duration before retry; false indicates immediate is ok
func (d *DynamoDB) handleError(err error, errorPrefix ...string) *DynamoDBError {
	if err != nil {
		prefix := ""

		if len(errorPrefix) > 0 {
			prefix = errorPrefix[0] + " "
		}

		prefixType := ""

		if aerr, ok := err.(awserr.Error); ok {
			// aws errors
			prefixType = "[AWS] "

			switch aerr.Code() {
			case dynamodb.ErrCodeConditionalCheckFailedException:
				fallthrough
			case dynamodb.ErrCodeResourceInUseException:
				fallthrough
			case dynamodb.ErrCodeResourceNotFoundException:
				fallthrough
			case dynamodb.ErrCodeIdempotentParameterMismatchException:
				fallthrough
			case dynamodb.ErrCodeBackupInUseException:
				fallthrough
			case dynamodb.ErrCodeBackupNotFoundException:
				fallthrough
			case dynamodb.ErrCodeContinuousBackupsUnavailableException:
				fallthrough
			case dynamodb.ErrCodeGlobalTableAlreadyExistsException:
				fallthrough
			case dynamodb.ErrCodeGlobalTableNotFoundException:
				fallthrough
			case dynamodb.ErrCodeIndexNotFoundException:
				fallthrough
			case dynamodb.ErrCodeInvalidRestoreTimeException:
				fallthrough
			case dynamodb.ErrCodePointInTimeRecoveryUnavailableException:
				fallthrough
			case dynamodb.ErrCodeReplicaAlreadyExistsException:
				fallthrough
			case dynamodb.ErrCodeReplicaNotFoundException:
				fallthrough
			case dynamodb.ErrCodeTableAlreadyExistsException:
				fallthrough
			case dynamodb.ErrCodeTableInUseException:
				fallthrough
			case dynamodb.ErrCodeTableNotFoundException:
				fallthrough
			case dynamodb.ErrCodeTransactionCanceledException:
				fallthrough
			case dynamodb.ErrCodeTransactionConflictException:
				fallthrough
			case dynamodb.ErrCodeTransactionInProgressException:
				// show error + no retry
				return &DynamoDBError{
					ErrorMessage: prefix + prefixType + aerr.Code() + " - " + aerr.Message(),
					SuppressError: false,
					AllowRetry: false,
					RetryNeedsBackOff: false,
				}

			case dynamodb.ErrCodeItemCollectionSizeLimitExceededException:
				fallthrough
			case dynamodb.ErrCodeLimitExceededException:
				// show error + allow retry with backoff
				return &DynamoDBError{
					ErrorMessage: prefix + prefixType + aerr.Code() + " - " + aerr.Message(),
					SuppressError: false,
					AllowRetry: true,
					RetryNeedsBackOff: true,
				}

			case dynamodb.ErrCodeProvisionedThroughputExceededException:
				fallthrough
			case dynamodb.ErrCodeRequestLimitExceeded:
				// no error + allow retry with backoff
				return &DynamoDBError{
					ErrorMessage: prefix + prefixType + aerr.Code() + " - " + aerr.Message(),
					SuppressError: true,
					AllowRetry: true,
					RetryNeedsBackOff: true,
				}

			case dynamodb.ErrCodeInternalServerError:
				// no error + allow auto retry without backoff
				return &DynamoDBError{
					ErrorMessage: prefix + prefixType + aerr.Code() + " - " + aerr.Message(),
					SuppressError: true,
					AllowRetry: true,
					RetryNeedsBackOff: false,
				}

			default:
				return &DynamoDBError{
					ErrorMessage: prefix + prefixType + aerr.Code() + " - " + aerr.Message(),
					SuppressError: false,
					AllowRetry: false,
					RetryNeedsBackOff: false,
				}
			}
		} else {
			// other errors
			prefixType = "[General] "

			return &DynamoDBError{
				ErrorMessage: prefix + prefixType + err.Error(),
				SuppressError: false,
				AllowRetry: false,
				RetryNeedsBackOff: false,
			}
		}
	} else {
		// no error
		return nil
	}
}

// Connect will establish a connection to the dynamodb service
func (d *DynamoDB) Connect() error {
	// clean up prior cn reference
	d.cn = nil
	d.SkipDax = false

	if !d.AwsRegion.Valid() || d.AwsRegion == awsregion.UNKNOWN {
		return errors.New("Connect To DynamoDB Failed: (AWS Session Error) " + "Region is Required")
	}

	// create custom http2 client if needed
	var httpCli *http.Client
	var httpErr error

	if d.HttpOptions == nil {
		d.HttpOptions = new(awshttp2.HttpClientSettings)
	}

	// use custom http2 client
	h2 := &awshttp2.AwsHttp2Client{
		Options: d.HttpOptions,
	}

	if httpCli, httpErr = h2.NewHttp2Client(); httpErr != nil {
		return errors.New("Connect to DynamoDB Failed: (AWS Session Error) " + "Create Custom Http2 Client Errored = " + httpErr.Error())
	}

	// establish aws session connection and connect to dynamodb service
	if sess, err := session.NewSession(
		&aws.Config{
			Region:      aws.String(d.AwsRegion.Key()),
			HTTPClient:  httpCli,
		}); err != nil {
		// aws session error
		return errors.New("Connect To DynamoDB Failed: (AWS Session Error) " + err.Error())
	} else {
		// aws session obtained
		d.cn = dynamodb.New(sess)

		if d.cn == nil {
			return errors.New("Connect To DynamoDB Failed: (New DynamoDB Connection) " + "Connection Object Nil")
		}

		// successfully connected to dynamodb service
		return nil
	}
}

// EnableDax will enable dax service for this dynamodb session
func (d *DynamoDB) EnableDax() error {
	if d.cn == nil {
		return errors.New("Enable Dax Failed: " + "DynamoDB Not Yet Connected")
	}

	cfg := dax.DefaultConfig()
	cfg.HostPorts = []string{ d.DaxEndpoint }
	cfg.Region = d.AwsRegion.Key()

	var err error

	d.cnDax, err = dax.New(cfg)

	if err != nil {
		d.cnDax = nil
		return errors.New("Enable Dax Failed: " + err.Error())
	}

	// default skip dax to false
	d.SkipDax = false

	// success
	return nil
}

// DisableDax will disable dax service for this dynamodb session
func (d *DynamoDB) DisableDax() {
	if d.cnDax != nil {
		_ = d.cnDax.Close()
		d.cnDax = nil
		d.SkipDax = false
	}
}

// do_PutItem is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_PutItem(input *dynamodb.PutItemInput, ctx ...aws.Context) (output *dynamodb.PutItemOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.PutItem(input)
		} else {
			return d.cnDax.PutItemWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.PutItem(input)
		} else {
			return d.cn.PutItemWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB PutItem Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_UpdateItem is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_UpdateItem(input *dynamodb.UpdateItemInput, ctx ...aws.Context) (output *dynamodb.UpdateItemOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.UpdateItem(input)
		} else {
			return d.cnDax.UpdateItemWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.UpdateItem(input)
		} else {
			return d.cn.UpdateItemWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB UpdateItem Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_DeleteItem is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_DeleteItem(input *dynamodb.DeleteItemInput, ctx ...aws.Context) (output *dynamodb.DeleteItemOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.DeleteItem(input)
		} else {
			return d.cnDax.DeleteItemWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.DeleteItem(input)
		} else {
			return d.cn.DeleteItemWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB DeleteItem Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_GetItem is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_GetItem(input *dynamodb.GetItemInput, ctx ...aws.Context) (output *dynamodb.GetItemOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.GetItem(input)
		} else {
			return d.cnDax.GetItemWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.GetItem(input)
		} else {
			return d.cn.GetItemWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB GetItem Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_Query is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_Query(input *dynamodb.QueryInput, pagedQuery bool, pagedQueryPageCountLimit *int64, ctx ...aws.Context) (output *dynamodb.QueryOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if !pagedQuery {
			//
			// not paged query
			//
			if len(ctx) <= 0 {
				return d.cnDax.Query(input)
			} else {
				return d.cnDax.QueryWithContext(ctx[0], input)
			}
		} else {
			//
			// paged query
			//
			pageCount := int64(0)

			fn := func(pageOutput *dynamodb.QueryOutput, lastPage bool) bool {
				if pageOutput != nil {
					if pageOutput.Items != nil && len(pageOutput.Items) > 0 {
						pageCount++

						if output == nil {
							output = new(dynamodb.QueryOutput)
						}

						output.SetCount(aws.Int64Value(output.Count) + aws.Int64Value(pageOutput.Count))
						output.SetScannedCount(aws.Int64Value(output.ScannedCount) + aws.Int64Value(pageOutput.ScannedCount))
						output.SetLastEvaluatedKey(pageOutput.LastEvaluatedKey)

						for _, v := range pageOutput.Items {
							output.Items = append(output.Items, v)
						}

						// check if ok to stop
						if pagedQueryPageCountLimit != nil && *pagedQueryPageCountLimit > 0 {
							if pageCount >= *pagedQueryPageCountLimit {
								return false
							}
						}
					}
				}

				return !lastPage
			}

			if len(ctx) <= 0 {
				err = d.cnDax.QueryPages(input, fn)
			} else {
				err = d.cnDax.QueryPagesWithContext(ctx[0], input, fn)
			}

			return output, err
		}
	} else if d.cn != nil {
		// dynamodb
		if !pagedQuery {
			//
			// not paged query
			//
			if len(ctx) <= 0 {
				return d.cn.Query(input)
			} else {
				return d.cn.QueryWithContext(ctx[0], input)
			}
		} else {
			//
			// paged query
			//
			pageCount := int64(0)

			fn := func(pageOutput *dynamodb.QueryOutput, lastPage bool) bool {
				if pageOutput != nil {
					if pageOutput.Items != nil && len(pageOutput.Items) > 0 {
						pageCount++

						if output == nil {
							output = new(dynamodb.QueryOutput)
						}

						output.SetCount(aws.Int64Value(output.Count) + aws.Int64Value(pageOutput.Count))
						output.SetScannedCount(aws.Int64Value(output.ScannedCount) + aws.Int64Value(pageOutput.ScannedCount))
						output.SetLastEvaluatedKey(pageOutput.LastEvaluatedKey)

						for _, v := range pageOutput.Items {
							output.Items = append(output.Items, v)
						}

						// check if ok to stop
						if pagedQueryPageCountLimit != nil && *pagedQueryPageCountLimit > 0 {
							if pageCount >= *pagedQueryPageCountLimit {
								return false
							}
						}
					}
				}

				return !lastPage
			}

			if len(ctx) <= 0 {
				err = d.cn.QueryPages(input, fn)
			} else {
				err = d.cn.QueryPagesWithContext(ctx[0], input, fn)
			}

			return output, err
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB QueryItems Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_Scan is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_Scan(input *dynamodb.ScanInput, pagedQuery bool, pagedQueryPageCountLimit *int64, ctx ...aws.Context) (output *dynamodb.ScanOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if !pagedQuery {
			//
			// not paged query
			//
			if len(ctx) <= 0 {
				return d.cnDax.Scan(input)
			} else {
				return d.cnDax.ScanWithContext(ctx[0], input)
			}
		} else {
			//
			// paged query
			//
			pageCount := int64(0)

			fn := func(pageOutput *dynamodb.ScanOutput, lastPage bool) bool {
				if pageOutput != nil {
					if pageOutput.Items != nil && len(pageOutput.Items) > 0 {
						pageCount++

						if output == nil {
							output = new(dynamodb.ScanOutput)
						}

						output.SetCount(aws.Int64Value(output.Count) + aws.Int64Value(pageOutput.Count))
						output.SetScannedCount(aws.Int64Value(output.ScannedCount) + aws.Int64Value(pageOutput.ScannedCount))
						output.SetLastEvaluatedKey(pageOutput.LastEvaluatedKey)

						for _, v := range pageOutput.Items {
							output.Items = append(output.Items, v)
						}

						if pagedQueryPageCountLimit != nil && *pagedQueryPageCountLimit > 0 {
							if pageCount >= *pagedQueryPageCountLimit {
								return false
							}
						}
					}
				}

				return !lastPage
			}

			if len(ctx) <= 0 {
				err = d.cnDax.ScanPages(input, fn)
			} else {
				err = d.cnDax.ScanPagesWithContext(ctx[0], input, fn)
			}

			return output, err
		}
	} else if d.cn != nil {
		// dynamodb
		if !pagedQuery {
			//
			// not paged query
			//
			if len(ctx) <= 0 {
				return d.cn.Scan(input)
			} else {
				return d.cn.ScanWithContext(ctx[0], input)
			}
		} else {
			//
			// paged query
			//
			pageCount := int64(0)

			fn := func(pageOutput *dynamodb.ScanOutput, lastPage bool) bool {
				if pageOutput != nil {
					if pageOutput.Items != nil && len(pageOutput.Items) > 0 {
						pageCount++

						if output == nil {
							output = new(dynamodb.ScanOutput)
						}

						output.SetCount(aws.Int64Value(output.Count) + aws.Int64Value(pageOutput.Count))
						output.SetScannedCount(aws.Int64Value(output.ScannedCount) + aws.Int64Value(pageOutput.ScannedCount))
						output.SetLastEvaluatedKey(pageOutput.LastEvaluatedKey)

						for _, v := range pageOutput.Items {
							output.Items = append(output.Items, v)
						}

						if pagedQueryPageCountLimit != nil && *pagedQueryPageCountLimit > 0 {
							if pageCount >= *pagedQueryPageCountLimit {
								return false
							}
						}
					}
				}

				return !lastPage
			}

			if len(ctx) <= 0 {
				err = d.cn.ScanPages(input, fn)
			} else {
				err = d.cn.ScanPagesWithContext(ctx[0], input, fn)
			}

			return output, err
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB ScanItems Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_BatchWriteItem is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_BatchWriteItem(input *dynamodb.BatchWriteItemInput, ctx ...aws.Context) (output *dynamodb.BatchWriteItemOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.BatchWriteItem(input)
		} else {
			return d.cnDax.BatchWriteItemWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.BatchWriteItem(input)
		} else {
			return d.cn.BatchWriteItemWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB BatchWriteItem Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_BatchGetItem is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_BatchGetItem(input *dynamodb.BatchGetItemInput, ctx ...aws.Context) (output *dynamodb.BatchGetItemOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.BatchGetItem(input)
		} else {
			return d.cnDax.BatchGetItemWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.BatchGetItem(input)
		} else {
			return d.cn.BatchGetItemWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB BatchGetItem Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_TransactWriteItems is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_TransactWriteItems(input *dynamodb.TransactWriteItemsInput, ctx ...aws.Context) (output *dynamodb.TransactWriteItemsOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.TransactWriteItems(input)
		} else {
			return d.cnDax.TransactWriteItemsWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.TransactWriteItems(input)
		} else {
			return d.cn.TransactWriteItemsWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB TransactionWriteItems Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// do_TransactGetItems is a helper that calls either dax or dynamodb based on dax availability
func (d *DynamoDB) do_TransactGetItems(input *dynamodb.TransactGetItemsInput, ctx ...aws.Context) (output *dynamodb.TransactGetItemsOutput, err error) {
	if d.cnDax != nil && !d.SkipDax {
		// dax
		if len(ctx) <= 0 {
			return d.cnDax.TransactGetItems(input)
		} else {
			return d.cnDax.TransactGetItemsWithContext(ctx[0], input)
		}
	} else if d.cn != nil {
		// dynamodb
		if len(ctx) <= 0 {
			return d.cn.TransactGetItems(input)
		} else {
			return d.cn.TransactGetItemsWithContext(ctx[0], input)
		}
	} else {
		// connection error
		return nil, errors.New("DynamoDB TransactionGetItems Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// PutItem will add a new item into dynamodb table
//
// parameters:
//		item = required, must be a struct object; ALWAYS SINGLE STRUCT OBJECT, NEVER SLICE
//			   must start with fields 'pk string', 'sk string', and 'data string' before any other attributes
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//
// notes:
//		item struct tags
//			use `json:"" dynamodbav:""`
//				json = sets the name used in json
//				dynamodbav = sets the name used in dynamodb
//			reference child element
//				if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//					Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) PutItem(item interface{}, timeOutDuration *time.Duration) *DynamoDBError {
	if d.cn == nil {
		return d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	if item == nil {
		return d.handleError(errors.New("DynamoDB PutItem Failed: " + "Input Item Object is Nil"))
	}

	if av, err := dynamodbattribute.MarshalMap(item); err != nil {
		return d.handleError(err, "DynamoDB PutItem Failed: (MarshalMap)")
	} else {
		input := &dynamodb.PutItemInput{
			Item: av,
			TableName: aws.String(d.TableName),
		}

		// record params payload
		d.LastExecuteParamsPayload = "PutItem = " + input.String()

		// save into dynamodb table
		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
			defer cancel()
			_, err = d.do_PutItem(input, ctx)
		} else {
			_, err = d.do_PutItem(input)
		}

		if err != nil {
			return d.handleError(err, "DynamoDB PutItem Failed: (PutItem)")
		}
	}

	// put item was successful
	return nil
}

// UpdateItem will update dynamodb item in given table using primary key (PK, SK), and set specific attributes with new value and persists
// UpdateItem requires using Primary Key attributes, and limited to TWO key attributes in condition maximum;
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//		pkValue = required, value of partition key to seek
//		skValue = optional, value of sort key to seek; set to blank if value not provided
//
//		updateExpression = required, ATTRIBUTES ARE CASE SENSITIVE; set remove add or delete action expression, see Rules URL for full detail
//			Rules:
//				1) https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Expressions.UpdateExpressions.html
//			Usage Syntax:
//				1) Action Keywords are: set, add, remove, delete
//				2) Each Action Keyword May Appear in UpdateExpression Only Once
//				3) Each Action Keyword Grouping May Contain One or More Actions, Such as 'set price=:p, age=:age, etc' (each action separated by comma)
//				4) Each Action Keyword Always Begin with Action Keyword itself, such as 'set ...', 'add ...', etc
//				5) If Attribute is Numeric, Action Can Perform + or - Operation in Expression, such as 'set age=age-:newAge, price=price+:price, etc'
//				6) If Attribute is Slice, Action Can Perform Slice Element Operation in Expression, such as 'set age[2]=:newData, etc'
//				7) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//					a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//				8) When Attribute is a List, Use list_append(a, b, ...) in Expression to append elements (list_append() is case sensitive)
//					a) set #ri = list_append(#ri, :vals) where :vals represents one or more of elements to add as in L
//				9) if_not_exists(path, value)
//					a) Avoids existing attribute if already exists
//					b) set price = if_not_exists(price, :p)
//					c) if_not_exists is case sensitive; path is the existing attribute to check
//				10) Action Type Purposes
//					a) SET = add one or more attributes to an item; overrides existing attributes in item with new values; if attribute is number, able to perform + or - operations
//					b) REMOVE = remove one or more attributes from an item, to remove multiple attributes, separate by comma; remove element from list use xyz[1] index notation
//					c) ADD = adds a new attribute and its values to an item; if attribute is number and already exists, value will add up or subtract
//					d) DELETE = supports only on set data types; deletes one or more elements from a set, such as 'delete color :c'
//				11) Example
//					a) set age=:age, name=:name, etc
//					b) set age=age-:age, num=num+:num, etc
//
//		conditionExpress = optional, ATTRIBUTES ARE CASE SENSITIVE; sets conditions for this condition expression, set to blank if not used
//				Usage Syntax:
//					1) "size(info.actors) >= :num"
//						a) When Length of Actors Attribute Value is Equal or Greater Than :num, ONLY THEN UpdateExpression is Performed
//					2) ExpressionAttributeName and ExpressionAttributeValue is Still Defined within ExpressionAttributeNames and ExpressionAttributeValues Where Applicable
//
//		expressionAttributeNames = optional, ATTRIBUTES ARE CASE SENSITIVE; set nil if not used, must define for attribute names that are reserved keywords such as year, data etc. using #xyz
//			Usage Syntax:
//				1) map[string]*string: where string is the #xyz, and *string is the original xyz attribute name
//					a) map[string]*string { "#xyz": aws.String("Xyz"), }
//				2) Add to Map
//					a) m := make(map[string]*string)
//					b) m["#xyz"] = aws.String("Xyz")
//
//		expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//			Usage Syntax:
//				1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//					a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//				2) Add to Map
//					a) m := make(map[string]*dynamodb.AttributeValue)
//					b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//				3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//					a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//					b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
//
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//
// notes:
//		item struct tags
//			use `json:"" dynamodbav:""`
//				json = sets the name used in json
//				dynamodbav = sets the name used in dynamodb
//			reference child element
//				if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//					Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) UpdateItem(pkValue string, skValue string,
							  updateExpression string,
							  conditionExpression string,
							  expressionAttributeNames map[string]*string,
							  expressionAttributeValues map[string]*dynamodb.AttributeValue,
							  timeOutDuration *time.Duration) *DynamoDBError {
	if d.cn == nil {
		return d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// validate input parameters
	if util.LenTrim(d.PKName) <= 0 {
		return d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "PK Name is Required"))
	}

	if util.LenTrim(pkValue) <= 0 {
		return d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "PK Value is Required"))
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			return d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "SK Name is Required"))
		}
	}

	if util.LenTrim(updateExpression) <= 0 {
		return d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "UpdateExpression is Required"))
	}

	if expressionAttributeValues == nil {
		return d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "ExpressionAttributeValues is Required"))
	}

	// define key
	m := make(map[string]*dynamodb.AttributeValue)

	m[d.PKName] = &dynamodb.AttributeValue{S: aws.String(pkValue)}

	if util.LenTrim(skValue) > 0 {
		m[d.SKName] = &dynamodb.AttributeValue{S: aws.String(skValue)}
	}

	// build update item input params
	params := &dynamodb.UpdateItemInput{
		TableName:                 aws.String(d.TableName),
		Key:                       m,
		UpdateExpression:          aws.String(updateExpression),
		ExpressionAttributeValues: expressionAttributeValues,
		ReturnValues:              aws.String(dynamodb.ReturnValueAllNew),
	}

	if util.LenTrim(conditionExpression) > 0 {
		params.ConditionExpression = aws.String(conditionExpression)
	}

	if expressionAttributeNames != nil {
		params.ExpressionAttributeNames = expressionAttributeNames
	}

	// record params payload
	d.LastExecuteParamsPayload = "UpdateItem = " + params.String()

	// execute dynamodb service
	var err error

	// create timeout context
	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		_, err = d.do_UpdateItem(params, ctx)
	} else {
		_, err = d.do_UpdateItem(params)
	}

	if err != nil {
		return d.handleError(err, "DynamoDB UpdateItem Failed: (UpdateItem)")
	}

	// update item successful
	return nil
}

// DeleteItem will delete an existing item from dynamodb table, using primary key values (PK and SK)
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//		pkValue = required, value of partition key to seek
//		skValue = optional, value of sort key to seek; set to blank if value not provided
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
func (d *DynamoDB) DeleteItem(pkValue string, skValue string,
							  timeOutDuration *time.Duration) *DynamoDBError {
	if d.cn	== nil {
		return d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	if util.LenTrim(d.PKName) <= 0 {
		return d.handleError(errors.New("DynamoDB DeleteItem Failed: " + "PK Name is Required"))
	}

	if util.LenTrim(pkValue) <= 0 {
		return d.handleError(errors.New("DynamoDB DeleteItem Failed: " + "PK Value is Required"))
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			return d.handleError(errors.New("DynamoDB DeleteItem Failed: " + "SK Name is Required"))
		}
	}

	m := make(map[string]*dynamodb.AttributeValue)

	m[d.PKName] = &dynamodb.AttributeValue{ S: aws.String(pkValue) }

	if util.LenTrim(skValue) > 0 {
		m[d.SKName] = &dynamodb.AttributeValue{ S: aws.String(skValue) }
	}

	params := &dynamodb.DeleteItemInput{
		TableName: aws.String(d.TableName),
		Key: m,
	}

	// record params payload
	d.LastExecuteParamsPayload = "DeleteItem = " + params.String()

	var err error

	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		_, err = d.do_DeleteItem(params, ctx)
	} else {
		_, err = d.do_DeleteItem(params)
	}

	if err != nil {
		return d.handleError(err, "DynamoDB DeleteItem Failed: (DeleteItem)")
	}

	// delete item was successful
	return nil
}

// GetItem will find an existing item from dynamodb table
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//		resultItemPtr = required, pointer to item object for return value to unmarshal into; if projected attributes less than struct fields, unmatched is defaulted
//			a) MUST BE STRUCT OBJECT; NEVER A SLICE
//		pkValue = required, value of partition key to seek
//		skValue = optional, value of sort key to seek; set to blank if value not provided
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//		consistentRead = optional, scan uses consistent read or eventual consistent read, default is eventual consistent read
//		projectedAttributes = optional; ATTRIBUTES ARE CASE SENSITIVE; variadic list of attribute names that this query will project into result items;
//						      attribute names must match struct field name or struct tag's json / dynamodbav tag values
//
// notes:
//		item struct tags
//			use `json:"" dynamodbav:""`
//				json = sets the name used in json
//				dynamodbav = sets the name used in dynamodb
//			reference child element
//				if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//					Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) GetItem(resultItemPtr interface{},
						   pkValue string, skValue string,
						   timeOutDuration *time.Duration, consistentRead *bool, projectedAttributes ...string) *DynamoDBError {
	if d.cn	== nil {
		return d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// validate input parameters
	if resultItemPtr == nil {
		return d.handleError(errors.New("DynamoDB GetItem Failed: " + "ResultItemPtr Must Initialize First"))
	}

	if util.LenTrim(d.PKName) <= 0 {
		return d.handleError(errors.New("DynamoDB GetItem Failed: " + "PK Name is Required"))
	}

	if util.LenTrim(pkValue) <= 0 {
		return d.handleError(errors.New("DynamoDB GetItem Failed: " + "PK Value is Required"))
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			return d.handleError(errors.New("DynamoDB GetItem Failed: " + "SK Name is Required"))
		}
	}

	// define key filter
	m := make(map[string]*dynamodb.AttributeValue)

	m[d.PKName] = &dynamodb.AttributeValue{ S: aws.String(pkValue) }

	if util.LenTrim(skValue) > 0 {
		m[d.SKName] = &dynamodb.AttributeValue{ S: aws.String(skValue) }
	}

	// define projected attributes
	var proj expression.ProjectionBuilder
	projSet := false

	if len(projectedAttributes) > 0 {
		// compose projected attributes if specified
		firstProjectedAttribute := expression.Name(projectedAttributes[0])
		moreProjectedAttributes := []expression.NameBuilder{}

		if len(projectedAttributes) > 1 {
			firstAttribute := true

			for _, v := range projectedAttributes {
				if !firstAttribute {
					moreProjectedAttributes = append(moreProjectedAttributes, expression.Name(v))
				} else {
					firstAttribute = false
				}
			}
		}

		if len(moreProjectedAttributes) > 0 {
			proj = expression.NamesList(firstProjectedAttribute, moreProjectedAttributes...)
		} else {
			proj = expression.NamesList(firstProjectedAttribute)
		}

		projSet = true
	}

	// compose filter expression and projection if applicable
	var expr expression.Expression
	var err error

	if projSet {
		if expr, err = expression.NewBuilder().WithProjection(proj).Build(); err != nil {
			return d.handleError(err, "DynamoDB GetItem Failed: (GetItem)")
		}
	}

	// set params
	params := &dynamodb.GetItemInput{
		TableName: aws.String(d.TableName),
		Key: m,
	}

	if projSet {
		params.ProjectionExpression = expr.Projection()
	}

	if consistentRead != nil {
		if *consistentRead {
			params.ConsistentRead = consistentRead
		}
	}

	// record params payload
	d.LastExecuteParamsPayload = "GetItem = " + params.String()

	// execute get item action
	var result *dynamodb.GetItemOutput

	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		result, err = d.do_GetItem(params, ctx)
	} else {
		result, err = d.do_GetItem(params)
	}

	// evaluate result
	if err != nil {
		return d.handleError(err, "DynamoDB GetItem Failed: (GetItem)")
	}

	if result == nil {
		return d.handleError(errors.New("DynamoDB GetItem Failed: " + "Result Object Nil"))
	}

	if err = dynamodbattribute.UnmarshalMap(result.Item, resultItemPtr); err != nil {
		return d.handleError(err, "DynamoDB GetItem Failed: (Unmarshal)")
	}

	// get item was successful
	return nil
}

// QueryItems will query dynamodb items in given table using primary key (PK, SK for example), or one of Global/Local Secondary Keys (indexName must be defined if using GSI)
// To query against non-key attributes, use Scan (bad for performance however)
// QueryItems requires using Key attributes, and limited to TWO key attributes in condition maximum;
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//		resultItemsPtr = required, pointer to items list struct to contain queried result; i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//		consistentRead = optional, scan uses consistent read or eventual consistent read, default is eventual consistent read
//		indexName = optional, global secondary index or local secondary index name to help in scan operation
//		pageLimit = optional, scan page limit if set, this limits number of items examined per page during scan operation, allowing scan to work better for RCU
//		pagedQuery = optional, indicates if query is page based or not; if true, query will be performed via pages, this helps overcome 1 MB limit of each query result
//		pagedQueryPageCountLimit = optional, indicates how many pages to query during paged query action
//		exclusiveStartKey = optional, if using pagedQuery and starting the query from prior results
//
//		keyConditionExpression = required, ATTRIBUTES ARE CASE SENSITIVE; either the primary key (PK SK for example) or global secondary index (SK Data for example) or another secondary index (secondary index must be named)
//			Usage Syntax:
//				1) Max 2 Attribute Fields
//				2) First Field must be Partition Key (Must Evaluate to True or False)
//					a) = ONLY
//				3) Second Field is Sort Key (May Evaluate to True or False or Range)
//					a) =, <, <=, >, >=, BETWEEN, begins_with()
//				4) Combine Two Fields with AND
//				5) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//					a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//				6) Example
//					a) partitionKeyName = :partitionKeyVal
//					b) partitionKeyName = :partitionKeyVal AND sortKeyName = :sortKeyVal
//					c) #yr = :year
//				7) If Using GSI / Local Index
//					a) When Using, Must Specify the IndexName
//					b) First Field is the GSI's Partition Key, such as SK (Evals to True/False), While Second Field is the GSI's SortKey (Range)
//
//		expressionAttributeNames = optional, ATTRIBUTES ARE CASE SENSITIVE; set nil if not used, must define for attribute names that are reserved keywords such as year, data etc. using #xyz
//			Usage Syntax:
//				1) map[string]*string: where string is the #xyz, and *string is the original xyz attribute name
//					a) map[string]*string { "#xyz": aws.String("Xyz"), }
//				2) Add to Map
//					a) m := make(map[string]*string)
//					b) m["#xyz"] = aws.String("Xyz")
//
//		expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//			Usage Syntax:
//				1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//					a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//				2) Add to Map
//					a) m := make(map[string]*dynamodb.AttributeValue)
//					b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//				3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//					a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//					b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
//
//		filterConditionExpression = optional; ATTRIBUTES ARE CASE SENSITIVE; once query on key conditions returned, this filter condition further restricts return data before output to caller;
//			Usage Syntax:
//				1) &expression.Name(xyz).Equals(expression.Value(abc))
//				2) &expression.Name(xyz).Equals(expression.Value(abc)).And(...)
//
//		projectedAttributes = optional; ATTRIBUTES ARE CASE SENSITIVE; variadic list of attribute names that this query will project into result items;
//						      attribute names must match struct field name or struct tag's json / dynamodbav tag values
//
// Return Values:
//		prevEvalKey = if paged query, the last evaluate key returned, to be used in subsequent query via exclusiveStartKey; otherwise always nil is returned
//
// notes:
//		item struct tags
//			use `json:"" dynamodbav:""`
//				json = sets the name used in json
//				dynamodbav = sets the name used in dynamodb
//			reference child element
//				if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//					Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) QueryItems(resultItemsPtr interface{},
							  timeOutDuration *time.Duration,
							  consistentRead *bool,
							  indexName *string,
							  pageLimit *int64,
							  pagedQuery bool,
							  pagedQueryPageCountLimit *int64,
							  exclusiveStartKey map[string]*dynamodb.AttributeValue,
							  keyConditionExpression string,
							  expressionAttributeNames map[string]*string,
							  expressionAttributeValues map[string]*dynamodb.AttributeValue,
							  filterConditionExpression *expression.ConditionBuilder,
							  projectedAttributes ...string) (prevEvalKey map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {
	if d.cn == nil {
		return nil, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return nil, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// result items pointer must be set
	if resultItemsPtr == nil {
		return nil, d.handleError(errors.New("DynamoDB QueryItems Failed: " + "ResultItems is Nil"))
	}

	// validate additional input parameters
	if util.LenTrim(keyConditionExpression) <= 0 {
		return nil, d.handleError(errors.New("DynamoDB QueryItems Failed: " + "KeyConditionExpress is Required"))
	}

	if expressionAttributeValues == nil {
		return nil, d.handleError(errors.New("DynamoDB QueryItems Failed: " + "ExpressionAttributeValues is Required"))
	}

	// gather projection attributes
	var proj expression.ProjectionBuilder
	projSet := false

	if len(projectedAttributes) > 0 {
		// compose projected attributes if specified
		firstProjectedAttribute := expression.Name(projectedAttributes[0])
		moreProjectedAttributes := []expression.NameBuilder{}

		if len(projectedAttributes) > 1 {
			firstAttribute := true

			for _, v := range projectedAttributes {
				if !firstAttribute {
					moreProjectedAttributes = append(moreProjectedAttributes, expression.Name(v))
				} else {
					firstAttribute = false
				}
			}
		}

		if len(moreProjectedAttributes) > 0 {
			proj = expression.NamesList(firstProjectedAttribute, moreProjectedAttributes...)
		} else {
			proj = expression.NamesList(firstProjectedAttribute)
		}

		projSet = true
	}

	// compose filter expression and projection if applicable
	var expr expression.Expression
	var err error

	filterSet := false

	if filterConditionExpression != nil && projSet {
		expr, err = expression.NewBuilder().WithFilter(*filterConditionExpression).WithProjection(proj).Build()
		filterSet = true
		projSet = true
	} else if filterConditionExpression != nil {
		expr, err = expression.NewBuilder().WithFilter(*filterConditionExpression).Build()
		filterSet = true
		projSet	 = false
	} else if projSet {
		expr, err = expression.NewBuilder().WithProjection(proj).Build()
		filterSet = false
		projSet	 = true
	}

	if err != nil {
		return nil, d.handleError(err, "DynamoDB QueryItems Failed (Filter/Projection Expression Build)")
	}

	// build query input params
	params := &dynamodb.QueryInput{
		TableName: aws.String(d.TableName),
		KeyConditionExpression: aws.String(keyConditionExpression),
		ExpressionAttributeValues: expressionAttributeValues,
	}

	if expressionAttributeNames != nil {
		params.ExpressionAttributeNames = expressionAttributeNames
	}

	if filterSet {
		params.FilterExpression = expr.Filter()

		if params.ExpressionAttributeNames == nil {
			params.ExpressionAttributeNames = make(map[string]*string)
		}

		for k, v := range expr.Names() {
			params.ExpressionAttributeNames[k] = v
		}

		for k, v := range expr.Values() {
			params.ExpressionAttributeValues[k] = v
		}
	}

	if projSet {
		params.ProjectionExpression = expr.Projection()
	}

	if consistentRead != nil {
		if *consistentRead {
			if len(*indexName) > 0 {
				// gsi not valid for consistent read, turn off consistent read
				*consistentRead = false
			}
		}

		params.ConsistentRead = consistentRead
	}

	if indexName != nil {
		params.IndexName = indexName
	}

	if pageLimit != nil {
		params.Limit = pageLimit
	}

	if exclusiveStartKey != nil {
		params.ExclusiveStartKey = exclusiveStartKey
	}

	// record params payload
	d.LastExecuteParamsPayload = "QueryItems = " + params.String()

	// execute dynamodb service
	var result *dynamodb.QueryOutput

	// create timeout context
	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		result, err = d.do_Query(params, pagedQuery, pagedQueryPageCountLimit, ctx)
	} else {
		result, err = d.do_Query(params, pagedQuery, pagedQueryPageCountLimit)
	}

	if err != nil {
		return nil, d.handleError(err, "DynamoDB QueryItems Failed: (QueryItems)")
	}

	// unmarshal result items to target object map
	if err = dynamodbattribute.UnmarshalListOfMaps(result.Items, resultItemsPtr); err != nil {
		return nil, d.handleError(err, "Dynamo QueryItems Failed: (Unmarshal Result Items)")
	}

	// query items successful
	return result.LastEvaluatedKey, nil
}

// ScanItems will scan dynamodb items in given table, project results, using filter expression
// >>> DO NOT USE SCAN IF POSSIBLE - SCAN IS NOT EFFICIENT ON RCU <<<
//
// parameters:
//		resultItemsPtr = required, pointer to items list struct to contain queried result; i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//		consistentRead = optional, scan uses consistent read or eventual consistent read, default is eventual consistent read
//		indexName = optional, global secondary index or local secondary index name to help in scan operation
//		pageLimit = optional, scan page limit if set, this limits number of items examined per page during scan operation, allowing scan to work better for RCU
//		pagedQuery = optional, indicates if query is page based or not; if true, query will be performed via pages, this helps overcome 1 MB limit of each query result
//		pagedQueryPageCountLimit = optional, indicates how many pages to query during paged query action
//		exclusiveStartKey = optional, if using pagedQuery and starting the query from prior results
//
//		filterConditionExpression = required; ATTRIBUTES ARE CASE SENSITIVE; sets the scan filter condition;
//			Usage Syntax:
//				1) &expression.Name(xyz).Equals(expression.Value(abc))
//				2) &expression.Name(xyz).Equals(expression.Value(abc)).And(...)
//
//		projectedAttributes = optional; ATTRIBUTES ARE CASE SENSITIVE; variadic list of attribute names that this query will project into result items;
//						      attribute names must match struct field name or struct tag's json / dynamodbav tag values
//
// Return Values:
//		prevEvalKey = if paged query, the last evaluate key returned, to be used in subsequent query via exclusiveStartKey; otherwise always nil is returned
//
// notes:
//		item struct tags
//			use `json:"" dynamodbav:""`
//				json = sets the name used in json
//				dynamodbav = sets the name used in dynamodb
//			reference child element
//				if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//					Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) ScanItems(resultItemsPtr interface{},
							 timeOutDuration *time.Duration,
							 consistentRead *bool,
							 indexName *string,
							 pageLimit *int64,
							 pagedQuery bool,
							 pagedQueryPageCountLimit *int64,
							 exclusiveStartKey map[string]*dynamodb.AttributeValue,
							 filterConditionExpression expression.ConditionBuilder, projectedAttributes ...string) (prevEvalKey map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {
	if d.cn == nil {
		return nil, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return nil, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// result items pointer must be set
	if resultItemsPtr == nil {
		return nil, d.handleError(errors.New("DynamoDB ScanItems Failed: " + "ResultItems is Nil"))
	}

	// create projected attributes
	var proj expression.ProjectionBuilder
	projSet := false

	if len(projectedAttributes) > 0 {
		firstProjectedAttribute := expression.Name(projectedAttributes[0])
		moreProjectedAttributes := []expression.NameBuilder{}

		if len(projectedAttributes) > 1 {
			firstAttribute := true

			for _, v := range projectedAttributes {
				if !firstAttribute {
					moreProjectedAttributes = append(moreProjectedAttributes, expression.Name(v))
				} else {
					firstAttribute = false
				}
			}
		}

		if len(moreProjectedAttributes) > 0 {
			proj = expression.NamesList(firstProjectedAttribute, moreProjectedAttributes...)
		} else {
			proj = expression.NamesList(firstProjectedAttribute)
		}

		projSet	= true
	}

	// build expression
	var expr expression.Expression
	var err error

	if projSet {
		expr, err = expression.NewBuilder().WithFilter(filterConditionExpression).WithProjection(proj).Build()
	} else {
		expr, err = expression.NewBuilder().WithFilter(filterConditionExpression).Build()
	}

	if err != nil {
		return nil, d.handleError(err, "DynamoDB ScanItems Failed: (Expression NewBuilder)")
	}

	// build query input params
	params := &dynamodb.ScanInput{
		TableName:                 aws.String(d.TableName),
		FilterExpression:          expr.Filter(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	}

	if projSet {
		params.ProjectionExpression = expr.Projection()
	}

	if consistentRead != nil {
		if *consistentRead {
			if len(*indexName) > 0 {
				// gsi not valid for consistent read, turn off consistent read
				*consistentRead = false
			}
		}

		params.ConsistentRead = consistentRead
	}

	if indexName != nil {
		params.IndexName = indexName
	}

	if pageLimit != nil {
		params.Limit = pageLimit
	}

	if exclusiveStartKey != nil {
		params.ExclusiveStartKey = exclusiveStartKey
	}

	// record params payload
	d.LastExecuteParamsPayload = "ScanItems = " + params.String()

	// execute dynamodb service
	var result *dynamodb.ScanOutput

	// create timeout context
	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		result, err = d.do_Scan(params, pagedQuery, pagedQueryPageCountLimit, ctx)
	} else {
		result, err = d.do_Scan(params, pagedQuery, pagedQueryPageCountLimit)
	}

	if err != nil {
		return nil, d.handleError(err, "DynamoDB ScanItems Failed: (ScanItems)")
	}

	// unmarshal result items to target object map
	if err = dynamodbattribute.UnmarshalListOfMaps(result.Items, resultItemsPtr); err != nil {
		return nil, d.handleError(err, "Dynamo ScanItems Failed: (Unmarshal Result Items)")
	}

	// scan items successful
	return result.LastEvaluatedKey, nil
}

// BatchWriteItems will group up to 25 put and delete items in a single batch, and perform actions in parallel against dynamodb for better write efficiency，
// To update items, use UpdateItem instead for each item needing to be updated instead, BatchWriteItems does not support update items
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//		putItems = slice of item struct objects to add to table (combine of putItems and deleteItems cannot exceed 25)
//			1) Each element of slice is an struct object to be added, struct object must have PK, SK or another named primary key for example, and other attributes as needed
//			2) putItems interface{} = Expects SLICE of STRUCT OBJECTS
//
//		deleteKeys = slice of search keys (as defined by DynamoDBTableKeys struct) to remove from table (combine of putItems and deleteKeys cannot exceed 25)
//			1) Each element of slice is an struct object of DynamoDBTableKeys
//
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//
// return values:
//		successCount = total number of item actions succeeded
//		unprocessedItems = any item actions did not succeed is returned; nil means all processed
//		err = if method call failed, error is returned
//
// notes:
//		item struct tags
//			use `json:"" dynamodbav:""`
//				json = sets the name used in json
//				dynamodbav = sets the name used in dynamodb
//			reference child element
//				if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//					Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) BatchWriteItems(putItems interface{},
								   deleteKeys []DynamoDBTableKeys,
								   timeOutDuration *time.Duration) (successCount int, unprocessedItems *DynamoDBUnprocessedItemsAndKeys, err *DynamoDBError) {
	if d.cn == nil {
		return 0, nil, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return 0, nil, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// validate input parameters
	if putItems == nil && deleteKeys == nil {
		return 0, nil, d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Both Cannot Be Nil"))
	}

	// marshal put and delete objects
	var putItemsAv []map[string]*dynamodb.AttributeValue
	var deleteKeysAv []map[string]*dynamodb.AttributeValue

	if putItems != nil {
		// putItems is in interface
		// need to reflect into slice of interface{}
		putItemsIf := util.SliceObjectsToSliceInterface(putItems)

		if putItemsIf != nil && len(putItemsIf) > 0 {
			for _, v := range putItemsIf {
				if m, e := dynamodbattribute.MarshalMap(v); e != nil {
					return 0, nil, d.handleError(e, "DynamoDB BatchWriteItems Failed: (PutItems MarshalMap)")
				} else {
					if m != nil {
						putItemsAv = append(putItemsAv, m)
					} else {
						return 0, nil, d.handleError(errors.New("DynamoDB BatchWriteItems Failed: (PutItems MarshalMap) " + "PutItem Marshal Result Object Nil"))
					}
				}
			}
		}
	}

	if deleteKeys != nil {
		if len(deleteKeys) > 0 {
			for _, v := range deleteKeys {
				if m, e := dynamodbattribute.MarshalMap(v); e != nil {
					return 0, nil, d.handleError(e, "DynamoDB BatchWriteItems Failed: (DeleteKeys MarshalMap)")
				} else {
					if m != nil {
						deleteKeysAv = append(deleteKeysAv, m)
					} else {
						return 0, nil, d.handleError(errors.New("DynamoDB BatchWriteItems Failed: (DeleteKeys MarshalMap) " + "DeleteKey Marshal Result Object Nil"))
					}
				}
			}
		}
	}

	putCount := 0
	deleteCount := 0

	if putItemsAv != nil {
		putCount = len(putItemsAv)
	}

	if deleteKeysAv != nil {
		deleteCount = len(deleteKeysAv)
	}

	if (putCount+deleteCount) <= 0 || (putCount+deleteCount) > 25 {
		return 0,nil, d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Count Must Be 1 to 25 Only"))
	}

	// holder of delete and put item write requests
	var writeRequests []*dynamodb.WriteRequest

	// define requestItems wrapper
	if deleteCount > 0 {
		for _, v := range deleteKeysAv {
			writeRequests = append(writeRequests, &dynamodb.WriteRequest{
				DeleteRequest: &dynamodb.DeleteRequest{
					Key: v,
				},
			})
		}
	}

	if putCount > 0 {
		for _, v := range putItemsAv {
			writeRequests = append(writeRequests, &dynamodb.WriteRequest{
				PutRequest: &dynamodb.PutRequest{
					Item: v,
				},
			})
		}
	}

	// compose batch write params
	params := &dynamodb.BatchWriteItemInput{
		RequestItems: map[string][]*dynamodb.WriteRequest{
			d.TableName: writeRequests,
		},
	}

	// record params payload
	d.LastExecuteParamsPayload = "BatchWriteItems = " + params.String()

	// execute batch write action
	var result *dynamodb.BatchWriteItemOutput
	var err1 error

	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		result, err1 = d.do_BatchWriteItem(params, ctx)
	} else {
		result, err1 = d.do_BatchWriteItem(params)
	}

	if err1 != nil {
		return 0,nil, d.handleError(err1, "DynamoDB BatchWriteItems Failed: (BatchWriteItem)")
	}

	// evaluate results
	unprocessed := result.UnprocessedItems

	if unprocessed != nil {
		list := unprocessed[d.TableName]

		if list != nil && len(list) > 0 {
			outList := new(DynamoDBUnprocessedItemsAndKeys)

			for _, v := range list {
				if v.PutRequest != nil && v.PutRequest.Item != nil {
					outList.PutItems = append(outList.PutItems, v.PutRequest.Item)
				}

				if v.DeleteRequest != nil && v.DeleteRequest.Key != nil {
					var o DynamoDBTableKeys

					if e := dynamodbattribute.UnmarshalMap(v.DeleteRequest.Key, &o); e == nil {
						outList.DeleteKeys = append(outList.DeleteKeys, &o)
					}
				}
			}

			return deleteCount+putCount-len(list), outList, nil
		}
	}

	// batch put and delete items successful
	return deleteCount+putCount, nil, nil
}

// BatchGetItems accepts a slice of search keys (of DynamoDBSearchKeys struct object), optionally define attribute projections, and return found result items;
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//		resultItemsPtr = required, pointer to items list struct to contain queried result; i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted
//		searchKeys = required, slice of DynamoDBTableKeys struct objects to perform search against
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//		consistentRead = optional, indicates if the read operation requires consistent read status
//		projectedAttributes = optional; ATTRIBUTES ARE CASE SENSITIVE; variadic list of attribute names that this query will project into result items;
//						      attribute names must match struct field name or struct tag's json / dynamodbav tag values
//
// return values:
//		notFound = true if no items found; if error encountered, this field returns false with error field filled
//		err = if error is encountered, this field will be filled; otherwise nil
//
// notes:
//		item struct tags
//			use `json:"" dynamodbav:""`
//				json = sets the name used in json
//				dynamodbav = sets the name used in dynamodb
//			reference child element
//				if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//					Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) BatchGetItems(resultItemsPtr interface{},
								 searchKeys []DynamoDBTableKeys,
								 timeOutDuration *time.Duration,
								 consistentRead *bool,
								 projectedAttributes ...string) (notFound bool, err *DynamoDBError) {
	if d.cn == nil {
		return false, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return false, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// validate input parameters
	if resultItemsPtr == nil {
		return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "ResultItems is Nil"))
	}

	if searchKeys == nil {
		return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "SearchKeys Cannot Be Nil"))
	}

	if len(searchKeys) <= 0 {
		return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "SearchKeys Required"))
	}

	if len(searchKeys) > 100 {
		return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "SearchKeys Maximum is 100"))
	}

	// marshal search keys into slice of map of dynamodb attribute values
	var keysAv []map[string]*dynamodb.AttributeValue

	for _, v := range searchKeys {
		if m, e := dynamodbattribute.MarshalMap(v); e != nil {
			return false, d.handleError(e, "DynamoDB BatchGetItems Failed: (SearchKey Marshal)")
		} else {
			if m != nil {
				keysAv = append(keysAv, m)
			} else {
				return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: (SearchKey Marshal) " + "Marshaled Result Nil"))
			}
		}
	}

	// define projected fields
	// define projected attributes
	var proj expression.ProjectionBuilder
	projSet := false

	if len(projectedAttributes) > 0 {
		// compose projected attributes if specified
		firstProjectedAttribute := expression.Name(projectedAttributes[0])
		moreProjectedAttributes := []expression.NameBuilder{}

		if len(projectedAttributes) > 1 {
			firstAttribute := true

			for _, v := range projectedAttributes {
				if !firstAttribute {
					moreProjectedAttributes = append(moreProjectedAttributes, expression.Name(v))
				} else {
					firstAttribute = false
				}
			}
		}

		if len(moreProjectedAttributes) > 0 {
			proj = expression.NamesList(firstProjectedAttribute, moreProjectedAttributes...)
		} else {
			proj = expression.NamesList(firstProjectedAttribute)
		}

		projSet = true
	}

	var expr expression.Expression
	var err1 error

	if projSet {
		if expr, err1 = expression.NewBuilder().WithProjection(proj).Build(); err1 != nil {
			return false, d.handleError(err1, "DynamoDB BatchGetItems Failed: (Projecting Attributes)")
		}
	}

	// define params
	params := &dynamodb.BatchGetItemInput{
		RequestItems: map[string]*dynamodb.KeysAndAttributes{
			d.TableName: {
				Keys: keysAv,
			},
		},
	}

	if projSet {
		params.RequestItems[d.TableName].ProjectionExpression = expr.Projection()
	}

	if consistentRead != nil {
		if *consistentRead {
			params.RequestItems[d.TableName].ConsistentRead = consistentRead
		}
	}

	// record params payload
	d.LastExecuteParamsPayload = "BatchGetItems = " + params.String()

	// execute batch
	var result *dynamodb.BatchGetItemOutput

	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		result, err1 = d.do_BatchGetItem(params, ctx)
	} else {
		result, err1 = d.do_BatchGetItem(params)
	}

	// evaluate batch execute result
	if err1 != nil {
		return false, d.handleError(err1, "DynamoDB BatchGetItems Failed: (BatchGetItem)")
	}

	if result.Responses == nil {
		// not found
		return true, nil
	} else {
		// retrieve items found for the given table name
		x := result.Responses[d.TableName]

		if x == nil {
			return true, nil
		} else {
			// unmarshal results
			if err1 = dynamodbattribute.UnmarshalListOfMaps(x, resultItemsPtr); err1 != nil {
				return false, d.handleError(err1, "DynamoDB BatchGetItems Failed: (Unmarshal ResultItems)")
			} else {
				// unmarshal successful
				return false, nil
			}
		}
	}
}

// TransactionWriteItems performs a transaction write action for one or more DynamoDBTransactionWrites struct objects,
// Either all success or all fail,
// Total Items Count in a Single Transaction for All transItems combined (inner elements) cannot exceed 25
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
func (d *DynamoDB) TransactionWriteItems(timeOutDuration *time.Duration, tranItems ...*DynamoDBTransactionWrites) (success bool, err *DynamoDBError) {
	if d.cn == nil {
		return false, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return false, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	if util.LenTrim(d.PKName) <= 0 {
		return false, d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: " + "PK Name is Required"))
	}

	if len(tranItems) == 0 {
		return false, d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: " + "Minimum of 1 TranItems is Required"))
	}

	// create working data
	var items []*dynamodb.TransactWriteItem

	// loop through all tranItems slice to pre-populate transaction write items slice
	skOK := false

	for _, t := range tranItems {
		tableName := t.TableNameOverride

		if util.LenTrim(tableName) <= 0 {
			tableName = d.TableName
		}

		if t.DeleteItems != nil && len(t.DeleteItems) > 0 {
			for _, v := range t.DeleteItems {
				m := new(dynamodb.TransactWriteItem)

				md := make(map[string]*dynamodb.AttributeValue)
				md[d.PKName] = &dynamodb.AttributeValue{ S: aws.String(v.PK) }

				if util.LenTrim(v.SK) > 0 {
					if !skOK {
						if util.LenTrim(d.SKName) <= 0 {
							return false, d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "SK Name is Required"))
						} else {
							skOK = true
						}
					}

					md[d.SKName] = &dynamodb.AttributeValue{ S: aws.String(v.SK) }
				}

				m.Delete = &dynamodb.Delete{
					TableName: aws.String(tableName),
					Key: md,
				}

				items = append(items, m)
			}
		}

		if t.PutItems != nil {
			if md, e := t.MarshalPutItems(); e != nil {
				return false, d.handleError(e, "DynamoDB TransactionWriteItems Failed: (Marshal PutItems)")
			} else {
				for _, v := range md {
					m := new(dynamodb.TransactWriteItem)

					m.Put = &dynamodb.Put{
						TableName: aws.String(tableName),
						Item: v,
					}

					items = append(items, m)
				}
			}
		}

		if t.UpdateItems != nil && len(t.UpdateItems) > 0 {
			for _, v := range t.UpdateItems {
				m := new(dynamodb.TransactWriteItem)

				mk := make(map[string]*dynamodb.AttributeValue)
				mk[d.PKName] = &dynamodb.AttributeValue{ S: aws.String(v.PK) }

				if util.LenTrim(v.SK) > 0 {
					if !skOK {
						if util.LenTrim(d.SKName) <= 0 {
							return false, d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "SK Name is Required"))
						} else {
							skOK = true
						}
					}

					mk[d.SKName] = &dynamodb.AttributeValue{ S: aws.String(v.SK) }
				}

				m.Update = &dynamodb.Update{
					TableName: aws.String(tableName),
					Key: mk,
				}

				if util.LenTrim(v.ConditionExpression) > 0 {
					m.Update.ConditionExpression = aws.String(v.ConditionExpression)
				}

				if util.LenTrim(v.UpdateExpression) > 0 {
					m.Update.UpdateExpression = aws.String(v.UpdateExpression)
				}

				if v.ExpressionAttributeNames != nil && len(v.ExpressionAttributeNames) > 0 {
					m.Update.ExpressionAttributeNames = v.ExpressionAttributeNames
				}

				if v.ExpressionAttributeValues != nil && len(v.ExpressionAttributeValues) > 0 {
					m.Update.ExpressionAttributeValues = v.ExpressionAttributeValues
				}

				items = append(items, m)
			}
		}
	}

	// items must not exceed 25
	if len(items) > 25 {
		return false, d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "Transaction Items May Not Exceed 25"))
	}

	if len(items) <= 0 {
		return false, d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "Transaction Items Minimum of 1 is Required"))
	}

	// compose transaction write items input var
	params := &dynamodb.TransactWriteItemsInput{
		TransactItems: items,
	}

	// record params payload
	d.LastExecuteParamsPayload = "TransactionWriteItems = " + params.String()

	// execute transaction write operation
	var err1 error

	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		_, err1 = d.do_TransactWriteItems(params, ctx)
	} else {
		_, err1 = d.do_TransactWriteItems(params)
	}

	if err1 != nil {
		return false, d.handleError(err1, "DynamoDB TransactionWriteItems Failed: (Transaction Canceled)")
	}

	// success
	return true, nil
}

// TransactionGetItems receives parameters via tranKeys variadic objects of type DynamoDBTransactionReads; each object has TableName override in case querying against other tables
// Each tranKeys struct object can contain one or more DynamoDBTableKeys struct, which contains PK, SK fields, and ResultItemPtr,
// The PK (required) and SK (optional) is used for search, while ResultItemPtr interface{} receives pointer to the output object, so that once query completes the appropriate item data will unmarshal into object
//
// important
//		if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// setting result item ptr info
//		1) Each DynamoDBTableKeys struct object must set pointer of output struct object to ResultItemPtr
//		2) In the external calling code, must define slice of struct object pointers to receive such unmarshaled results
//			a) output := []*MID{
//							&MID{},
//							&MID{},
//						 }
//			b) Usage
//				Passing each element of output to ResultItemPtr within DynamoDBTableKeys struct object
//
// notes:
//		1) transKeys' must contain at laest one object
//		2) within transKeys object, at least one object of DynamoDBTableKeys must exist for search
//		3) no more than total of 25 search keys allowed across all variadic objects
//		4) the ResultItemPtr in all DynamoDBTableKeys objects within all variadic objects MUST BE SET
func (d *DynamoDB) TransactionGetItems(timeOutDuration *time.Duration, tranKeys ...*DynamoDBTransactionReads) (successCount int, err *DynamoDBError) {
	if d.cn == nil {
		return 0, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return 0, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	if util.LenTrim(d.PKName) <= 0 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "PK Name is Required"))
	}

	if len(tranKeys) == 0 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "Minimum of 1 TranKeys is Required"))
	}

	// create working data
	var keys []*dynamodb.TransactGetItem
	var output []*DynamoDBTableKeys

	// loop through all tranKeys slice to pre-populate transaction get items key slice
	skOK := false

	for _, k := range tranKeys {
		tableName := k.TableNameOverride

		if util.LenTrim(tableName) <= 0 {
			tableName = d.TableName
		}

		if k.Keys != nil && len(k.Keys) > 0 {
			for _, v := range k.Keys {
				if v.ResultItemPtr == nil {
					return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "All SearchKeys Must Define Unmarshal Target Object"))
				} else {
					// add to output
					output = append(output, v)
				}

				m := new(dynamodb.TransactGetItem)

				md := make(map[string]*dynamodb.AttributeValue)
				md[d.PKName] = &dynamodb.AttributeValue{ S: aws.String(v.PK) }

				if util.LenTrim(v.SK) > 0 {
					if !skOK {
						if util.LenTrim(d.SKName) <= 0 {
							return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "SK Name is Required"))
						} else {
							skOK = true
						}
					}

					md[d.SKName] = &dynamodb.AttributeValue{ S: aws.String(v.SK) }
				}

				m.Get = &dynamodb.Get{
					TableName: aws.String(tableName),
					Key: md,
				}

				keys = append(keys, m)
			}
		}
	}

	// keys must not exceed 25
	if len(keys) > 25 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: (Payload Validate) " + "Search Keys May Not Exceed 25"))
	}

	if len(keys) <= 0 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: (Payload Validate) " + "Search Keys Minimum of 1 is Required"))
	}

	// compose transaction get items input var
	params := &dynamodb.TransactGetItemsInput{
		TransactItems: keys,
	}

	// record params payload
	d.LastExecuteParamsPayload = "TransactionGetItems = " + params.String()

	// execute transaction get operation
	var result *dynamodb.TransactGetItemsOutput
	var err1 error

	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		result, err1 = d.do_TransactGetItems(params, ctx)
	} else {
		result, err1 = d.do_TransactGetItems(params)
	}

	if err1 != nil {
		return 0, d.handleError(err1, "DynamoDB TransactionGetItems Failed: (Transaction Reads)")
	}

	// evaluate response
	successCount = 0

	if result.Responses != nil && len(result.Responses) > 0 {
		hasSK := util.LenTrim(d.SKName) > 0

		for _, v := range result.Responses {
			itemAv := v.Item

			if itemAv != nil {
				pk := util.Trim(aws.StringValue(itemAv[d.PKName].S))
				sk := ""

				if hasSK {
					sk = util.Trim(aws.StringValue(itemAv[d.SKName].S))
				}

				if len(pk) > 0 {
					// loop through output slice to match and unmarshal
					for _, o := range output {
						if !o.resultProcessed {
							found := false

							if len(sk) > 0 {
								// must match pk and sk
								if o.PK == pk && o.SK == sk && o.ResultItemPtr != nil {
									found = true
								}
							} else {
								// must match pk only
								if o.PK == pk && o.ResultItemPtr != nil {
									found = true
								}
							}

							if found {
								o.resultProcessed = true

								// unmarshal to object
								if e := dynamodbattribute.UnmarshalMap(itemAv, o.ResultItemPtr); e != nil {
									return 0, d.handleError(e, "DynamoDB TransactionGetItems Failed: (Unmarshal Result)")
								} else {
									// unmarshal successful
									successCount++
								}
							}
						}
					}
				}
			}
		}
	}

	// nothing found or something found, both returns nil for error
	return successCount, nil
}