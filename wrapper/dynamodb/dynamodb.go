package dynamodb

/*
 * Copyright 2020-2024 Aldelo, LP
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
//
// To Install & Setup AWS CLI on Host:
//		1) https://docs.aws.amazon.com/cli/latest/userguide/install-cliv2-linux.html
//				On Ubuntu, if host does not have zip and unzip:
//					$> sudo apt install zip
//					$> sudo apt install unzip
//				On Ubuntu, to install AWS CLI v2:
//					$> curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"
//					$> unzip awscliv2.zip
//					$> sudo ./aws/install
//		2) $> aws configure set region awsRegionName --profile default
// 		3) $> aws configure
//				follow prompts to enter Access ID and Secret Key
//
// AWS Region Name Reference:
//		us-west-2, us-east-1, ap-northeast-1, etc
//		See: https://docs.aws.amazon.com/general/latest/gr/rande.html
// =================================================================================================================

import (
	"context"
	"errors"
	"fmt"
	util "github.com/aldelo/common"
	awshttp2 "github.com/aldelo/common/wrapper/aws"
	"github.com/aldelo/common/wrapper/aws/awsregion"
	"github.com/aldelo/common/wrapper/xray"
	"github.com/aws/aws-dax-go/dax"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/dynamodb/expression"
	awsxray "github.com/aws/aws-xray-sdk-go/xray"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// *********************************************************************************************************************
// *********************************************************************************************************************
// *********************************************************************************************************************
//
// DYNAMODB WRAPPER HELPER STRUCTS
//
// *********************************************************************************************************************
// *********************************************************************************************************************
// *********************************************************************************************************************

// =====================================================================================================================
// DynamoDB Error Struct
// =====================================================================================================================

// DynamoDBError struct contains special status info including error and retry advise
type DynamoDBError struct {
	ErrorMessage  string
	SuppressError bool

	AllowRetry        bool
	RetryNeedsBackOff bool

	TransactionConditionalCheckFailed bool
}

// Error returns error string of the struct object
func (e *DynamoDBError) Error() string {
	return e.ErrorMessage
}

// =====================================================================================================================
// DynamoDB TableKeys Struct
// =====================================================================================================================

// DynamoDBTableKeys struct defines the PK and SK fields to be used in key search (Always PK and SK)
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// ResultItemPtr = optional, used with TransactionGetItems() to denote output unmarshal object target
type DynamoDBTableKeys struct {
	PK string // value
	SK string // value

	TableNameOverride string `dynamodbav:"-"` // if set, will override the table name
	PKNameOverride    string `dynamodbav:"-"` // if set, will override the PK name
	SKNameOverride    string `dynamodbav:"-"` // if set, will override the SK name

	ResultItemPtr interface{} `dynamodbav:"-"`
	ResultError   error       `dynamodbav:"-"`

	resultProcessed bool
}

type DynamoDBTableKeyValue struct {
	PK string // value
	SK string // value

	ResultError error
}

// =====================================================================================================================
// DynamoDB ConditionExpressionSet Struct
// =====================================================================================================================

// DynamoDBConditionExpressionSet struct defines the condition expression and its attribute values if any
type DynamoDBConditionExpressionSet struct {
	ConditionExpression       string
	ExpressionAttributeValues map[string]*dynamodb.AttributeValue
}

// =====================================================================================================================
// DynamoDB ProjectedAttributesSet Struct
// =====================================================================================================================

// DynamoDBProjectedAttributesSet struct defines a set of projected attributes and its table name
type DynamoDBProjectedAttributesSet struct {
	ProjectedAttributes []string
}

// BuildProjectionParameters will build the attribute projection parameters needed for the projection expression
func (a *DynamoDBProjectedAttributesSet) BuildProjectionParameters() (projectionExpression *string, expressionAttributeNames map[string]*string, err error) {
	// validate
	if a == nil {
		return nil, nil, errors.New("BuildProjectionParameters Failed: (Validate) " + "DynamoDBProjectedAttributesSet Object Nil")
	}

	if a.ProjectedAttributes == nil || (a.ProjectedAttributes != nil && len(a.ProjectedAttributes) <= 0) {
		// if no projected attributes, treat as project all
		return nil, nil, nil
	}

	// define projected attributes
	projectedAttributeNames := make([]expression.NameBuilder, 0)

	for i := 0; i < len(a.ProjectedAttributes); i++ {
		projectedAttributeNames = append(projectedAttributeNames, expression.Name(a.ProjectedAttributes[i]))
	}

	var proj expression.ProjectionBuilder

	if len(projectedAttributeNames) == 1 {
		proj = expression.NamesList(projectedAttributeNames[0])
	} else if len(projectedAttributeNames) > 1 {
		proj = expression.NamesList(projectedAttributeNames[0], projectedAttributeNames[1:]...)
	} else {
		// no projection, treat as project all
		return nil, nil, nil
	}

	// compose filter expression and projection if applicable
	var expr expression.Expression

	if expr, err = expression.NewBuilder().WithProjection(proj).Build(); err != nil {
		return nil, nil, errors.New("BuildProjectionParameters Failed: (Expression Build) " + err.Error())
	}

	// expression built, return result
	return expr.Projection(), expr.Names(), nil
}

// =====================================================================================================================
// DynamoDB MultiGetRequestResponse Struct
// =====================================================================================================================

// DynamoDBMultiGetRequestResponse struct defines container for request response properties,
// including for pointer to ResultItemsSlice, ItemsCount in ResultItemsSlice, and the associated Table Name,
// each Request Response is for a specific Table Name.
//
// !!! NOTE = When Participate in Slice, Table Name Must Not Duplicate !!!
//
// TableName = indicates the dynamodb table name that the ResultItemsSlicePtr is associated with
//
// PKName = given table's PK Name, typically 'PK'
// SKName = given table's SK Name, typically 'SK'
//
// SearchKeys = the PK SK values to search for the given table, accepts one or more pairs of PK SK values to search multiple records
// ProjectedAttributes = (optional) if response limited to certain attributes, specify attribute projection here, always include PK in the attribute projection if specified
//
// ConsistentRead = true if using consistent read, false or nil for eventual consistency
//
// ResultItemsSlicePtr = pointer to the slice of struct objects that will be unmarshaled into, for example, ResultItemsSlicePtr = &[]MyStruct{} or &[]*MyStruct{}
// ResultItemsCount = indicates the total result items count in the ResultItemsSlicePtr
type DynamoDBMultiGetRequestResponse struct {
	TableName string

	PKName string
	SKName string

	SearchKeys          []*DynamoDBTableKeyValue
	ProjectedAttributes *DynamoDBProjectedAttributesSet

	ConsistentRead *bool

	ResultItemsSlicePtr interface{}
	ResultItemsCount    int
}

// MarshalSearchKeyValueMaps will convert struct's SearchKeys into []map[string]*dynamodb.AttributeValue for use with dynamodb.KeysAndAttributes object
func (r *DynamoDBMultiGetRequestResponse) MarshalSearchKeyValueMaps() (result []map[string]*dynamodb.AttributeValue, err error) {
	if r == nil {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "DynamoDBMultiGetRequestResponse Object Nil")
	}

	if r.SearchKeys == nil {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SearchKeys Nil")
	}

	if len(r.SearchKeys) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SearchKeys Empty")
	}

	if r.SearchKeys[0] == nil {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SearchKeys[0] Nil")
	}

	if util.LenTrim(r.TableName) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "TableName Empty")
	}

	if util.LenTrim(r.PKName) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "PKName Empty")
	}

	if util.LenTrim(r.SearchKeys[0].SK) > 0 && util.LenTrim(r.SKName) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SKName Empty")
	}

	result = make([]map[string]*dynamodb.AttributeValue, 0)

	// loop thru each search key to marshal
	if util.LenTrim(r.SKName) > 0 {
		for _, kv := range r.SearchKeys {
			if kv != nil {
				result = append(result, map[string]*dynamodb.AttributeValue{
					r.PKName: {
						S: aws.String(kv.PK),
					},
					r.SKName: {
						S: aws.String(kv.SK),
					},
				})
			}
		}
	} else {
		for _, kv := range r.SearchKeys {
			if kv != nil {
				result = append(result, map[string]*dynamodb.AttributeValue{
					r.PKName: {
						S: aws.String(kv.PK),
					},
				})
			}
		}
	}

	if len(result) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Marshal) " + "Result Empty")
	}

	// return result
	return result, nil
}

// UnmarshalResultItems will convert struct's ResultItemsSlicePtr into target slice of struct objects
//
// ddbResultItemAttributes = required, the dynamodb result item attributes to unmarshal, comes from dynamodb BatchGetItem or TransactionGetItem or similar actions
func (r *DynamoDBMultiGetRequestResponse) UnmarshalResultItems(ddbResultItemAttributes []map[string]*dynamodb.AttributeValue) error {
	if r == nil {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "DynamoDBMultiGetRequestResponse Object Nil")
	}

	if r.ResultItemsSlicePtr == nil {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "ResultItemsSlicePtr Object Not Setup")
	}

	if reflect.TypeOf(r.ResultItemsSlicePtr).Kind() != reflect.Ptr {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "ResultItemsSlicePtr Must Be a Pointer")
	}

	if ddbResultItemAttributes == nil {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "Result Item Attributes From DDB is Nil")
	}

	if len(ddbResultItemAttributes) <= 0 {
		// no items to unmarshal
		r.ResultItemsCount = 0
		return nil
	}

	if err := dynamodbattribute.UnmarshalListOfMaps(ddbResultItemAttributes, r.ResultItemsSlicePtr); err != nil {
		return errors.New("UnmarshalResultItems Failed: (Unmarshal) " + err.Error())
	} else {
		// success
		r.ResultItemsCount = len(ddbResultItemAttributes)
		return nil
	}
}

// =====================================================================================================================
// DynamoDB UnprocessedItemsAndKeys Struct
// =====================================================================================================================

// DynamoDBUnprocessedItemsAndKeys defines struct to slices of items and keys
type DynamoDBUnprocessedItemsAndKeys struct {
	TableName  string
	PutItems   []map[string]*dynamodb.AttributeValue
	DeleteKeys []*DynamoDBTableKeys
}

// UnmarshalPutItems will convert struct's PutItems into target slice of struct objects
//
// notes:
//
//	resultItemsPtr interface{} = Input is Slice of Actual Struct Objects
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

// =====================================================================================================================
// DynamoDB UpdateItemInput Struct
// =====================================================================================================================

// DynamoDBUpdateItemInput defines a single item update instruction
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//
//	pkValue = required, value of partition key to seek
//	skValue = optional, value of sort key to seek; set to blank if value not provided
//
//	updateExpression = required, ATTRIBUTES ARE CASE SENSITIVE; set remove add or delete action expression, see Rules URL for full detail
//		Rules:
//			1) https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Expressions.UpdateExpressions.html
//		Usage Syntax:
//			1) Action Keywords are: set, add, remove, delete
//			2) Each Action Keyword May Appear in UpdateExpression Only Once
//			3) Each Action Keyword Grouping May Contain One or More Actions, Such as 'set price=:p, age=:age, etc' (each action separated by comma)
//			4) Each Action Keyword Always Begin with Action Keyword itself, such as 'set ...', 'add ...', etc
//			5) If Attribute is Numeric, Action Can Perform + or - Operation in Expression, such as 'set age=age-:newAge, price=price+:price, etc'
//			6) If Attribute is Slice, Action Can Perform Slice Element Operation in Expression, such as 'set age[2]=:newData, etc'
//			7) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//				a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//			8) When Attribute is a List, Use list_append(a, b, ...) in Expression to append elements (list_append() is case sensitive)
//				a) set #ri = list_append(#ri, :vals) where :vals represents one or more of elements to add as in L
//			9) if_not_exists(path, value)
//				a) Avoids existing attribute if already exists
//				b) set price = if_not_exists(price, :p)
//				c) if_not_exists is case sensitive; path is the existing attribute to check
//			10) Action Type Purposes
//				a) SET = add one or more attributes to an item; overrides existing attributes in item with new values; if attribute is number, able to perform + or - operations
//				b) REMOVE = remove one or more attributes from an item, to remove multiple attributes, separate by comma; remove element from list use xyz[1] index notation
//				c) ADD = adds a new attribute and its values to an item; if attribute is number and already exists, value will add up or subtract
//				d) DELETE = supports only on set data types; deletes one or more elements from a set, such as 'delete color :c'
//			11) Example
//				a) set age=:age, name=:name, etc
//				b) set age=age-:age, num=num+:num, etc
//
//	conditionExpress = optional, ATTRIBUTES ARE CASE SENSITIVE; sets conditions for this condition expression, set to blank if not used
//			Usage Syntax:
//				1) "size(info.actors) >= :num"
//					a) When Length of Actors Attribute Value is Equal or Greater Than :num, ONLY THEN UpdateExpression is Performed
//				2) ExpressionAttributeName and ExpressionAttributeValue is Still Defined within ExpressionAttributeNames and ExpressionAttributeValues Where Applicable
//
//	expressionAttributeNames = optional, ATTRIBUTES ARE CASE SENSITIVE; set nil if not used, must define for attribute names that are reserved keywords such as year, data etc. using #xyz
//		Usage Syntax:
//			1) map[string]*string: where string is the #xyz, and *string is the original xyz attribute name
//				a) map[string]*string { "#xyz": aws.String("Xyz"), }
//			2) Add to Map
//				a) m := make(map[string]*string)
//				b) m["#xyz"] = aws.String("Xyz")
//
//	expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//		Usage Syntax:
//			1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//				a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//			2) Add to Map
//				a) m := make(map[string]*dynamodb.AttributeValue)
//				b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//			3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//				a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//				b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
//
// TransactionOnly_TableNameOverride = optional, if set, will override the table name when using transaction only
type DynamoDBUpdateItemInput struct {
	PK                        string
	SK                        string
	UpdateExpression          string
	ConditionExpression       string
	ExpressionAttributeNames  map[string]*string
	ExpressionAttributeValues map[string]*dynamodb.AttributeValue
	TableNameOverride         string // if set, will override the table name when using transaction only
	PKNameOverride            string // if set, will override the PK name when using transaction only
	SKNameOverride            string // if set, will override the SK name when using transaction only
}

// =====================================================================================================================
// DynamoDB TransactionWritePutItemsSet Struct
// =====================================================================================================================

// DynamoDBTransactionWritePutItemsSet contains Slice of Put Items that are Struct (Value), NOT pointers to Structs,
// each DynamoDBTransactionWritePutItemsSet struct contains the same set of PutItems in terms of data schema.
//
// PutItems interface{} = Slice of Put Items that are Struct (Value), NOT pointers to Structs
//
//	*) Example: []MyStruct{}, NOT []*MyStruct{}
//
// ConditionExpression = optional, sets the condition expression for this put items, set to blank if not used
// ExpressionAttributeValues = optional, sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
// TableNameOverride = optional, if set, will override the table name when using transaction only
type DynamoDBTransactionWritePutItemsSet struct {
	PutItems                  interface{}
	ConditionExpression       string
	ExpressionAttributeValues map[string]*dynamodb.AttributeValue
	TableNameOverride         string
}

// MarshalPutItems will marshal dynamodb transaction writes put items into []map[string]*dynamodb.AttributeValue
func (p *DynamoDBTransactionWritePutItemsSet) MarshalPutItems() (result []map[string]*dynamodb.AttributeValue, err error) {
	if p == nil {
		return nil, errors.New("MarshalPutItems Failed: (Validate) " + "DynamoDBTransactionWritePutItemsSet Object Nil")
	}

	// validate
	if p.PutItems == nil {
		// no PutItems
		return nil, nil
	}

	// get []interface{}
	itemsIf := util.SliceObjectsToSliceInterface(p.PutItems)

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

// =====================================================================================================================
// DynamoDB TransactionWrites Struct
// =====================================================================================================================

// DynamoDBTransactionWrites defines one or more items to put, update or delete
//
// notes
//  1. PutItemsSet = Slice of DynamoDBTransactionWritePutItems Objects, which each contains its own data schema's PutItems and Condition Expression,
//     this way, we can support multiple sets of PutItems with different data schemas to be executed within the same transaction, even across tables.
//  2. UpdateItems = Slice of DynamoDBUpdateItemInput Objects, each object defines a single item update instruction
//  3. DeleteItems = Slice of DynamoDBTableKeys Objects, each object defines a single item delete instruction
type DynamoDBTransactionWrites struct {
	PutItemsSet []*DynamoDBTransactionWritePutItemsSet
	UpdateItems []*DynamoDBUpdateItemInput
	DeleteItems []*DynamoDBTableKeys

	allPutItems interface{}
}

// LoadPutItems will return all put items in all PutItemsSets,
// the acquired allPutItems will be stored into w.allPutItems for later use
func (w *DynamoDBTransactionWrites) LoadPutItems() interface{} {
	if w == nil {
		return nil
	}

	if w.PutItemsSet == nil {
		w.allPutItems = nil
		return nil
	}

	if len(w.PutItemsSet) <= 0 {
		w.allPutItems = nil
		return nil
	}

	// loop thru each put items set to get all put items
	var allPutItems interface{}

	for _, putItemsSet := range w.PutItemsSet {
		if putItemsSet != nil {
			if putItemsSet.PutItems != nil {
				if allPutItems == nil {
					allPutItems = putItemsSet.PutItems
				} else {
					allPutItems, _ = util.ReflectAppendSlices(allPutItems, putItemsSet.PutItems)
				}
			}
		}
	}

	// return result
	w.allPutItems = allPutItems
	return allPutItems
}

// GetPutItems will return allPutItems from w.allPutItems loaded via LoadPutItems()
func (w *DynamoDBTransactionWrites) GetPutItems() interface{} {
	if w == nil {
		return nil
	}

	if w.allPutItems == nil {
		return nil
	}

	return w.allPutItems
}

// =====================================================================================================================
// DynamoDB TransactionReads Struct
// =====================================================================================================================

// DynamoDBTransactionReads defines a set of get item search keys, with each holding result items slice pointer,
//
// !!! NOTE = When Participate in Slice, Table Name CAN Duplicate since DynamoDBTransactionReads holds slice rather than map, and result processing key lookup is by PK SK rather than table name !!!
type DynamoDBTransactionReads struct {
	TableName string

	PKName string
	SKName string

	SearchKeys          []*DynamoDBTableKeyValue
	ProjectedAttributes *DynamoDBProjectedAttributesSet

	ResultItemsSlicePtr interface{}
	ResultItemsCount    int

	resultItemKey []string
}

// MarshalSearchKeyValueMaps will convert struct's SearchKeys into []map[string]*dynamodb.AttributeValue
func (g *DynamoDBTransactionReads) MarshalSearchKeyValueMaps() (result []map[string]*dynamodb.AttributeValue, err error) {
	if g == nil {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "DynamoDBTransactionReads Object Nil")
	}

	if g.SearchKeys == nil {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SearchKeys Nil")
	}

	if len(g.SearchKeys) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SearchKeys Empty")
	}

	if g.SearchKeys[0] == nil {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SearchKeys[0] Nil")
	}

	if util.LenTrim(g.TableName) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "TableName Empty")
	}

	if util.LenTrim(g.PKName) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "PKName Empty")
	}

	if util.LenTrim(g.SearchKeys[0].SK) > 0 && util.LenTrim(g.SKName) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Validate) " + "SKName Empty")
	}

	result = make([]map[string]*dynamodb.AttributeValue, 0)

	// loop thru each search key to marshal
	if util.LenTrim(g.SKName) > 0 {
		for _, kv := range g.SearchKeys {
			if kv != nil {
				result = append(result, map[string]*dynamodb.AttributeValue{
					g.PKName: {
						S: aws.String(kv.PK),
					},
					g.SKName: {
						S: aws.String(kv.SK),
					},
				})

				// add to result item key hash
				g.resultItemKey = append(g.resultItemKey, strings.ToUpper(kv.PK+"."+kv.SK))
			}
		}
	} else {
		for _, kv := range g.SearchKeys {
			if kv != nil {
				result = append(result, map[string]*dynamodb.AttributeValue{
					g.PKName: {
						S: aws.String(kv.PK),
					},
				})

				// add to result item key hash
				g.resultItemKey = append(g.resultItemKey, strings.ToUpper(kv.PK+"."))
			}
		}
	}

	if len(result) <= 0 {
		return nil, errors.New("MarshalSearchKeyValueMaps Failed: (Marshal) " + "Result Empty")
	}

	// return result
	return result, nil
}

// UnmarshalResultItems will convert struct ResultItemsSlicePtr into target slice of struct objects
//
// itemResponses = Result from DynamoDB TransactionGetItems() which returns TransactionGetItemsOutput, within it is the Responses slice containing the []*ItemResponse
func (g *DynamoDBTransactionReads) UnmarshalResultItems(itemResponses []*dynamodb.ItemResponse) error {
	if g == nil {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "DynamoDBTransactionReads Object Nil")
	}

	if g.ResultItemsSlicePtr == nil {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "ResultItemsSlicePtr Object Not Setup")
	}

	if reflect.TypeOf(g.ResultItemsSlicePtr).Kind() != reflect.Ptr {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "ResultItemsSlicePtr Must Be a Pointer")
	}

	if itemResponses == nil {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "Item Responses From DDB is Nil")
	}

	if len(itemResponses) == 0 {
		// no items to unmarshal
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "Item Responses From DDB is Empty")
	}

	ddbResultItemAttributes := make([]map[string]*dynamodb.AttributeValue, 0)

	// loop thru itemKey to find matches from itemResponses, then extract the item attributes to ddbResultItemAttributes when matched
	skDefined := util.LenTrim(g.SKName) > 0

	for _, itemKey := range g.resultItemKey {
		for _, itemResponse := range itemResponses {
			if itemResponse != nil {
				if itemResponse.Item != nil {
					pkAttr := itemResponse.Item[g.PKName]

					var skAttr *dynamodb.AttributeValue
					if skDefined {
						skAttr = itemResponse.Item[g.SKName]
					}

					pkValue := ""
					skValue := ""

					if pkAttr != nil {
						pkValue = aws.StringValue(pkAttr.S)
					}

					if skAttr != nil {
						skValue = aws.StringValue(skAttr.S)
					}

					if strings.ToUpper(itemKey) == strings.ToUpper(pkValue+"."+skValue) {
						// match
						ddbResultItemAttributes = append(ddbResultItemAttributes, itemResponse.Item)
					}
				}
			}
		}
	}

	if ddbResultItemAttributes == nil {
		return errors.New("UnmarshalResultItems Failed: (Validate) " + "Result Item Attributes From DDB is Nil")
	}

	if len(ddbResultItemAttributes) <= 0 {
		// no items to unmarshal
		g.ResultItemsCount = 0
		return nil
	}

	if err := dynamodbattribute.UnmarshalListOfMaps(ddbResultItemAttributes, g.ResultItemsSlicePtr); err != nil {
		return errors.New("UnmarshalResultItems Failed: (Unmarshal) " + err.Error())
	} else {
		// success
		g.ResultItemsCount = len(ddbResultItemAttributes)
		return nil
	}
}

// *********************************************************************************************************************
// *********************************************************************************************************************
// *********************************************************************************************************************
//
// DYNAMODB WRAPPER STRUCT
//
// *********************************************************************************************************************
// *********************************************************************************************************************
// *********************************************************************************************************************

// =====================================================================================================================
// DynamoDB Wrapper Struct
// =====================================================================================================================

// DynamoDB struct encapsulates the AWS DynamoDB access functionality
//
// Notes:
//  1. to use dax, must be within vpc with dax cluster subnet pointing to private ip subnet of the vpc
//  2. dax is not accessible outside of vpc
//  3. on ec2 or container within vpc, also need aws credential via aws cli too = aws configure
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
	PKName    string
	SKName    string

	// last execute param string
	LastExecuteParamsPayload string

	_parentSegment *xray.XRayParentSegment
}

// =====================================================================================================================
// Internal Utility Helpers
// =====================================================================================================================

// handleError is an internal helper method to evaluate dynamodb error,
// and to advise if retry, immediate retry, suppress error etc error handling advisory
//
// notes:
//
//	RetryNeedsBackOff = true indicates when doing retry, must wait an arbitrary time duration before retry; false indicates immediate is ok
func (d *DynamoDB) handleError(err error, errorPrefix ...string) *DynamoDBError {
	if err != nil {
		prefix := ""

		if len(errorPrefix) > 0 {
			prefix = errorPrefix[0] + " "
		}

		prefixType := ""
		origError := ""

		if aerr, ok := err.(awserr.Error); ok {
			// aws errors
			prefixType = "[AWS] "

			if aerr.OrigErr() != nil {
				origError = "OrigErr = " + aerr.OrigErr().Error()
			} else {
				origError = "OrigErr = Nil"
			}

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
			case dynamodb.ErrCodeTransactionConflictException:
				fallthrough
			case dynamodb.ErrCodeTransactionInProgressException:
				// show error + no retry
				return &DynamoDBError{
					ErrorMessage:                      prefix + prefixType + aerr.Code() + " - " + aerr.Message() + " - " + origError,
					SuppressError:                     false,
					AllowRetry:                        false,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: false,
				}

			case dynamodb.ErrCodeTransactionCanceledException:
				// if ConditionalCheckFailed, then this may indicate duplicate, set TransactionConditionalCheckFailed status
				aerrStr := aerr.Message()
				transCondCheckFailed := false

				if strings.Contains(aerrStr, "ConditionalCheckFailed") {
					transCondCheckFailed = true
				}

				return &DynamoDBError{
					ErrorMessage:                      prefix + prefixType + aerr.Code() + " - " + aerrStr + " - " + origError,
					SuppressError:                     false,
					AllowRetry:                        false,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: transCondCheckFailed,
				}

			case dynamodb.ErrCodeItemCollectionSizeLimitExceededException:
				fallthrough
			case dynamodb.ErrCodeLimitExceededException:
				// show error + allow retry with backoff
				return &DynamoDBError{
					ErrorMessage:                      prefix + prefixType + aerr.Code() + " - " + aerr.Message() + " - " + origError,
					SuppressError:                     false,
					AllowRetry:                        true,
					RetryNeedsBackOff:                 true,
					TransactionConditionalCheckFailed: false,
				}

			case dynamodb.ErrCodeProvisionedThroughputExceededException:
				fallthrough
			case dynamodb.ErrCodeRequestLimitExceeded:
				// no error + allow retry with backoff
				return &DynamoDBError{
					ErrorMessage:                      prefix + prefixType + aerr.Code() + " - " + aerr.Message() + " - " + origError,
					SuppressError:                     true,
					AllowRetry:                        true,
					RetryNeedsBackOff:                 true,
					TransactionConditionalCheckFailed: false,
				}

			case dynamodb.ErrCodeInternalServerError:
				// no error + allow auto retry without backoff
				return &DynamoDBError{
					ErrorMessage:                      prefix + prefixType + aerr.Code() + " - " + aerr.Message() + " - " + origError,
					SuppressError:                     true,
					AllowRetry:                        true,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: false,
				}

			default:
				return &DynamoDBError{
					ErrorMessage:                      prefix + prefixType + aerr.Code() + " - " + aerr.Message() + " - " + origError,
					SuppressError:                     false,
					AllowRetry:                        false,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: false,
				}
			}
		} else {
			// other errors
			prefixType = "[General] "

			return &DynamoDBError{
				ErrorMessage:                      prefix + prefixType + err.Error(),
				SuppressError:                     false,
				AllowRetry:                        false,
				RetryNeedsBackOff:                 false,
				TransactionConditionalCheckFailed: false,
			}
		}
	} else {
		// no error
		return nil
	}
}

// =====================================================================================================================
// Public Utility Helpers
// =====================================================================================================================

// UpdateParentSegment updates this struct's xray parent segment, if no parent segment, set nil
func (d *DynamoDB) UpdateParentSegment(parentSegment *xray.XRayParentSegment) {
	d._parentSegment = parentSegment
}

// TimeOutDuration returns time.Duration pointer from timeOutSeconds
func (d *DynamoDB) TimeOutDuration(timeOutSeconds uint) *time.Duration {
	if timeOutSeconds == 0 {
		return nil
	} else {
		return util.DurationPtr(time.Duration(timeOutSeconds) * time.Second)
	}
}

// =====================================================================================================================
// Connect Functions
// =====================================================================================================================

// Connect will establish a connection to the dynamodb service
func (d *DynamoDB) Connect(parentSegment ...*xray.XRayParentSegment) (err error) {
	if xray.XRayServiceOn() {
		if len(parentSegment) > 0 {
			d._parentSegment = parentSegment[0]
		}

		seg := xray.NewSegment("DynamoDB-Connect", d._parentSegment)
		defer seg.Close()
		defer func() {
			_ = seg.Seg.AddMetadata("DynamoDB-AWS-Region", d.AwsRegion)
			_ = seg.Seg.AddMetadata("DynamoDB-Table-Name", d.TableName)

			if err != nil {
				_ = seg.Seg.AddError(err)
			}
		}()

		err = d.connectInternal()

		if err == nil {
			awsxray.AWS(d.cn.Client)
		}

		return err
	} else {
		return d.connectInternal()
	}
}

// Connect will establish a connection to the dynamodb service
func (d *DynamoDB) connectInternal() error {
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
			Region:     aws.String(d.AwsRegion.Key()),
			HTTPClient: httpCli,
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

// =====================================================================================================================
// Enable / Disable Dax Functions
// =====================================================================================================================

// EnableDax will enable dax service for this dynamodb session
func (d *DynamoDB) EnableDax() (err error) {
	// get new xray segment for tracing
	seg := xray.NewSegmentNullable("DynamoDB-EnableDax", d._parentSegment)

	if seg != nil {
		defer seg.Close()
		defer func() {
			_ = seg.Seg.AddMetadata("DynamoDB-Dax-Endpoint", d.DaxEndpoint)

			if err != nil {
				_ = seg.Seg.AddError(err)
			}
		}()

		err = d.enableDaxInternal()
		return err
	} else {
		return d.enableDaxInternal()
	}
}

// EnableDax will enable dax service for this dynamodb session
func (d *DynamoDB) enableDaxInternal() error {
	if d.cn == nil {
		return errors.New("Enable Dax Failed: " + "DynamoDB Not Yet Connected")
	}

	cfg := dax.DefaultConfig()
	cfg.HostPorts = []string{d.DaxEndpoint}
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

// =====================================================================================================================
// Internal do_PutItem Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_UpdateItem Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_DeleteItem Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_GetItem Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_Query_Pagination_Data Helper
// =====================================================================================================================

// do_Query_Pagination_Data is a helper that calls either dax or dynamodb based on dax availability, to get pagination data for the given query filter
func (d *DynamoDB) do_Query_Pagination_Data(input *dynamodb.QueryInput, ctx ...aws.Context) (paginationData []map[string]*dynamodb.AttributeValue, err error) {
	paginationData = make([]map[string]*dynamodb.AttributeValue, 0)

	if d.cnDax != nil && !d.SkipDax {
		// dax
		fn := func(pageOutput *dynamodb.QueryOutput, lastPage bool) bool {
			if pageOutput != nil {
				if pageOutput.Items != nil && len(pageOutput.Items) > 0 {
					if pageOutput.LastEvaluatedKey != nil {
						paginationData = append(paginationData, pageOutput.LastEvaluatedKey)
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

		return paginationData, err

	} else if d.cn != nil {
		// dynamodb
		fn := func(pageOutput *dynamodb.QueryOutput, lastPage bool) bool {
			if pageOutput != nil {
				if pageOutput.Items != nil && len(pageOutput.Items) > 0 {
					if pageOutput.LastEvaluatedKey != nil {
						paginationData = append(paginationData, pageOutput.LastEvaluatedKey)
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

		return paginationData, err

	} else {
		// connection error
		return nil, errors.New("DynamoDB QueryPaginationData Failed: " + "No DynamoDB or Dax Connection Available")
	}
}

// =====================================================================================================================
// Internal do_Query Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_Scan Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_BatchWriteItem Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_BatchGetItem Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_TransactionWriteItems Helper
// =====================================================================================================================

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

// =====================================================================================================================
// Internal do_TransactionGetItems Helper
// =====================================================================================================================

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

// =====================================================================================================================
// PutItem Functions
// =====================================================================================================================

// PutItem will add or update a new item into dynamodb table
//
// parameters:
//
//		item = required, must be a struct object; ALWAYS SINGLE STRUCT OBJECT, NEVER SLICE
//			   must start with fields 'pk string', 'sk string', and 'data string' before any other attributes
//		timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//	 conditionExpressionSet = optional, conditional expression to apply to the put item operation
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) PutItem(item interface{}, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) (ddbErr *DynamoDBError) {
	if xray.XRayServiceOn() {
		return d.putItemWithTrace(item, timeOutDuration, conditionExpressionSet...)
	} else {
		return d.putItemNormal(item, timeOutDuration, conditionExpressionSet...)
	}
}

func (d *DynamoDB) putItemWithTrace(item interface{}, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) (ddbErr *DynamoDBError) {
	trace := xray.NewSegment("DynamoDB-PutItem", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("DynamoDB Connection is Required"))
		return ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return ddbErr
	}

	if item == nil {
		ddbErr = d.handleError(errors.New("DynamoDB PutItem Failed: " + "Input Item Object is Nil"))
		return ddbErr
	}

	conditionExpressionStr := ""
	var conditionExpressionAttributeValues map[string]*dynamodb.AttributeValue

	if len(conditionExpressionSet) > 0 {
		if cond := conditionExpressionSet[0]; cond != nil {
			conditionExpressionStr = cond.ConditionExpression
			conditionExpressionAttributeValues = cond.ExpressionAttributeValues
		}
	}

	trace.Capture("PutItem", func() error {
		if av, err := dynamodbattribute.MarshalMap(item); err != nil {
			ddbErr = d.handleError(err, "DynamoDB PutItem Failed: (MarshalMap)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		} else {
			input := &dynamodb.PutItemInput{
				Item:      av,
				TableName: aws.String(d.TableName),
			}

			if util.LenTrim(conditionExpressionStr) > 0 {
				input.ConditionExpression = aws.String(conditionExpressionStr)

				if conditionExpressionAttributeValues != nil && len(conditionExpressionAttributeValues) > 0 {
					input.ExpressionAttributeValues = conditionExpressionAttributeValues
				}
			}

			// record params payload
			d.LastExecuteParamsPayload = "PutItem = " + input.String()

			subTrace := trace.NewSubSegment("PutItem_Do")
			defer subTrace.Close()

			// save into dynamodb table
			if timeOutDuration != nil {
				ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
				defer cancel()
				_, err = d.do_PutItem(input, ctx)
			} else {
				_, err = d.do_PutItem(input, subTrace.Ctx)
			}

			if err != nil {
				ddbErr = d.handleError(err, "DynamoDB PutItem Failed: (PutItem)")
				return fmt.Errorf(ddbErr.ErrorMessage)
			} else {
				return nil
			}
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":                 d.TableName,
			"ItemInfo":                  item,
			"ConditionExpression":       conditionExpressionStr,
			"ExpressionAttributeValues": conditionExpressionAttributeValues,
		},
	})

	// put item was successful
	return ddbErr
}

func (d *DynamoDB) putItemNormal(item interface{}, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) (ddbErr *DynamoDBError) {
	if d.cn == nil {
		return d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	if item == nil {
		return d.handleError(errors.New("DynamoDB PutItem Failed: " + "Input Item Object is Nil"))
	}

	conditionExpressionStr := ""
	var conditionExpressionAttributeValues map[string]*dynamodb.AttributeValue

	if len(conditionExpressionSet) > 0 {
		if cond := conditionExpressionSet[0]; cond != nil {
			conditionExpressionStr = cond.ConditionExpression
			conditionExpressionAttributeValues = cond.ExpressionAttributeValues
		}
	}

	if av, err := dynamodbattribute.MarshalMap(item); err != nil {
		ddbErr = d.handleError(err, "DynamoDB PutItem Failed: (MarshalMap)")
	} else {
		input := &dynamodb.PutItemInput{
			Item:      av,
			TableName: aws.String(d.TableName),
		}

		if util.LenTrim(conditionExpressionStr) > 0 {
			input.ConditionExpression = aws.String(conditionExpressionStr)

			if conditionExpressionAttributeValues != nil && len(conditionExpressionAttributeValues) > 0 {
				input.ExpressionAttributeValues = conditionExpressionAttributeValues
			}
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
			ddbErr = d.handleError(err, "DynamoDB PutItem Failed: (PutItem)")
		} else {
			ddbErr = nil
		}
	}

	// put item was successful
	return ddbErr
}

// PutItemWithRetry add or updates, and handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) PutItemWithRetry(maxRetries uint, item interface{}, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) *DynamoDBError {
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	if err := d.PutItem(item, util.DurationPtr(timeout), conditionExpressionSet...); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("PutItemWithRetry Failed: " + err.ErrorMessage)
				return d.PutItemWithRetry(maxRetries-1, item, util.DurationPtr(timeout), conditionExpressionSet...)
			} else {
				if err.SuppressError {
					log.Println("PutItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil
				} else {
					return &DynamoDBError{
						ErrorMessage:      "PutItemWithRetry Failed: " + err.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("PutItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil
			} else {
				return &DynamoDBError{
					ErrorMessage:      "PutItemWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		return nil
	}
}

// =====================================================================================================================
// UpdateItem Functions
// =====================================================================================================================

// UpdateItem will update dynamodb item in given table using primary key (PK, SK), and set specific attributes with new value and persists
// UpdateItem requires using Primary Key attributes, and limited to TWO key attributes in condition maximum;
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//
//	pkValue = required, value of partition key to seek
//	skValue = optional, value of sort key to seek; set to blank if value not provided
//
//	updateExpression = required, ATTRIBUTES ARE CASE SENSITIVE; set remove add or delete action expression, see Rules URL for full detail
//		Rules:
//			1) https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Expressions.UpdateExpressions.html
//		Usage Syntax:
//			1) Action Keywords are: set, add, remove, delete
//			2) Each Action Keyword May Appear in UpdateExpression Only Once
//			3) Each Action Keyword Grouping May Contain One or More Actions, Such as 'set price=:p, age=:age, etc' (each action separated by comma)
//			4) Each Action Keyword Always Begin with Action Keyword itself, such as 'set ...', 'add ...', etc
//			5) If Attribute is Numeric, Action Can Perform + or - Operation in Expression, such as 'set age=age-:newAge, price=price+:price, etc'
//			6) If Attribute is Slice, Action Can Perform Slice Element Operation in Expression, such as 'set age[2]=:newData, etc'
//			7) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//				a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//			8) When Attribute is a List, Use list_append(a, b, ...) in Expression to append elements (list_append() is case sensitive)
//				a) set #ri = list_append(#ri, :vals) where :vals represents one or more of elements to add as in L
//			9) if_not_exists(path, value)
//				a) Avoids existing attribute if already exists
//				b) set price = if_not_exists(price, :p)
//				c) if_not_exists is case sensitive; path is the existing attribute to check
//			10) Action Type Purposes
//				a) SET = add one or more attributes to an item; overrides existing attributes in item with new values; if attribute is number, able to perform + or - operations
//				b) REMOVE = remove one or more attributes from an item, to remove multiple attributes, separate by comma; remove element from list use xyz[1] index notation
//				c) ADD = adds a new attribute and its values to an item; if attribute is number and already exists, value will add up or subtract
//				d) DELETE = supports only on set data types; deletes one or more elements from a set, such as 'delete color :c'
//			11) Example
//				a) set age=:age, name=:name, etc
//				b) set age=age-:age, num=num+:num, etc
//
//	conditionExpress = optional, ATTRIBUTES ARE CASE SENSITIVE; sets conditions for this condition expression, set to blank if not used
//			Usage Syntax:
//				1) "size(info.actors) >= :num"
//					a) When Length of Actors Attribute Value is Equal or Greater Than :num, ONLY THEN UpdateExpression is Performed
//				2) ExpressionAttributeName and ExpressionAttributeValue is Still Defined within ExpressionAttributeNames and ExpressionAttributeValues Where Applicable
//
//	expressionAttributeNames = optional, ATTRIBUTES ARE CASE SENSITIVE; set nil if not used, must define for attribute names that are reserved keywords such as year, data etc. using #xyz
//		Usage Syntax:
//			1) map[string]*string: where string is the #xyz, and *string is the original xyz attribute name
//				a) map[string]*string { "#xyz": aws.String("Xyz"), }
//			2) Add to Map
//				a) m := make(map[string]*string)
//				b) m["#xyz"] = aws.String("Xyz")
//
//	expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//		Usage Syntax:
//			1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//				a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//			2) Add to Map
//				a) m := make(map[string]*dynamodb.AttributeValue)
//				b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//			3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//				a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//				b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
//
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) UpdateItem(pkValue string, skValue string,
	updateExpression string,
	conditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue,
	timeOutDuration *time.Duration) (ddbErr *DynamoDBError) {

	if xray.XRayServiceOn() {
		return d.updateItemWithTrace(pkValue, skValue, updateExpression, conditionExpression, expressionAttributeNames, expressionAttributeValues, timeOutDuration)
	} else {
		return d.updateItemNormal(pkValue, skValue, updateExpression, conditionExpression, expressionAttributeNames, expressionAttributeValues, timeOutDuration)
	}
}

func (d *DynamoDB) updateItemWithTrace(pkValue string, skValue string,
	updateExpression string,
	conditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue,
	timeOutDuration *time.Duration) (ddbErr *DynamoDBError) {

	trace := xray.NewSegment("DynamoDB-UpdateItem", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("DynamoDB Connection is Required"))
		return ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return ddbErr
	}

	// validate input parameters
	if util.LenTrim(d.PKName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "PK Name is Required"))
		return ddbErr
	}

	if util.LenTrim(pkValue) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "PK Value is Required"))
		return ddbErr
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			ddbErr = d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "SK Name is Required"))
			return ddbErr
		}
	}

	if util.LenTrim(updateExpression) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "UpdateExpression is Required"))
		return ddbErr
	}

	if expressionAttributeValues == nil {
		ddbErr = d.handleError(errors.New("DynamoDB UpdateItem Failed: " + "ExpressionAttributeValues is Required"))
		return ddbErr
	}

	trace.Capture("UpdateItem", func() error {
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

		subTrace := trace.NewSubSegment("UpdateItem_Do")
		defer subTrace.Close()

		// create timeout context
		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			_, err = d.do_UpdateItem(params, ctx)
		} else {
			_, err = d.do_UpdateItem(params, subTrace.Ctx)
		}

		if err != nil {
			ddbErr = d.handleError(err, "DynamoDB UpdateItem Failed: (UpdateItem)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		} else {
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":                 d.TableName,
			"PK":                        pkValue,
			"SK":                        skValue,
			"UpdateExpression":          updateExpression,
			"ConditionExpress":          conditionExpression,
			"ExpressionAttributeNames":  expressionAttributeNames,
			"ExpressionAttributeValues": expressionAttributeValues,
		},
	})

	// update item successful
	return ddbErr
}

func (d *DynamoDB) updateItemNormal(pkValue string, skValue string,
	updateExpression string,
	conditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue,
	timeOutDuration *time.Duration) (ddbErr *DynamoDBError) {

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
		ddbErr = d.handleError(err, "DynamoDB UpdateItem Failed: (UpdateItem)")
	} else {
		ddbErr = nil
	}

	// update item successful
	return ddbErr
}

// UpdateItemWithRetry handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) UpdateItemWithRetry(maxRetries uint,
	pkValue string, skValue string,
	updateExpression string,
	conditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue,
	timeOutDuration *time.Duration) *DynamoDBError {
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 10 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	} else if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}

	if err := d.UpdateItem(pkValue, skValue, updateExpression, conditionExpression, expressionAttributeNames, expressionAttributeValues, util.DurationPtr(timeout)); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("UpdateItemWithRetry Failed: " + err.ErrorMessage)
				return d.UpdateItemWithRetry(maxRetries-1, pkValue, skValue, updateExpression, conditionExpression, expressionAttributeNames, expressionAttributeValues, util.DurationPtr(timeout))
			} else {
				if err.SuppressError {
					log.Println("UpdateItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil
				} else {
					return &DynamoDBError{
						ErrorMessage:      "UpdateItemWithRetry Failed: " + err.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("UpdateItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil
			} else {
				return &DynamoDBError{
					ErrorMessage:      "UpdateItemWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		return nil
	}
}

// =====================================================================================================================
// RemoveItemAttribute Functions
// =====================================================================================================================

// RemoveItemAttribute will remove attribute from dynamodb item in given table using primary key (PK, SK)
func (d *DynamoDB) RemoveItemAttribute(pkValue string, skValue string, removeExpression string, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) (ddbErr *DynamoDBError) {
	if xray.XRayServiceOn() {
		return d.removeItemAttributeWithTrace(pkValue, skValue, removeExpression, timeOutDuration, conditionExpressionSet...)
	} else {
		return d.removeItemAttributeNormal(pkValue, skValue, removeExpression, timeOutDuration, conditionExpressionSet...)
	}
}

func (d *DynamoDB) removeItemAttributeWithTrace(pkValue string, skValue string, removeExpression string, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) (ddbErr *DynamoDBError) {
	trace := xray.NewSegment("DynamoDB-RemoveItemAttribute", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("DynamoDB Connection is Required"))
		return ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return ddbErr
	}

	// validate input parameters
	if util.LenTrim(d.PKName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "PK Name is Required"))
		return ddbErr
	}

	if util.LenTrim(pkValue) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "PK Value is Required"))
		return ddbErr
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			ddbErr = d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "SK Name is Required"))
			return ddbErr
		}
	}

	if util.LenTrim(removeExpression) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "RemoveExpression is Required"))
		return ddbErr
	}

	trace.Capture("RemoveItemAttribute", func() error {
		// define key
		m := make(map[string]*dynamodb.AttributeValue)

		m[d.PKName] = &dynamodb.AttributeValue{S: aws.String(pkValue)}

		if util.LenTrim(skValue) > 0 {
			m[d.SKName] = &dynamodb.AttributeValue{S: aws.String(skValue)}
		}

		// build update item input params for remove item attribute action
		params := &dynamodb.UpdateItemInput{
			TableName:        aws.String(d.TableName),
			Key:              m,
			UpdateExpression: aws.String(removeExpression),
			ReturnValues:     aws.String(dynamodb.ReturnValueAllNew),
		}

		if len(conditionExpressionSet) > 0 && conditionExpressionSet[0] != nil {
			conditionExpression := conditionExpressionSet[0].ConditionExpression

			if util.LenTrim(conditionExpression) > 0 {
				params.ConditionExpression = aws.String(conditionExpression)

				if conditionExpressionSet[0].ExpressionAttributeValues != nil && len(conditionExpressionSet[0].ExpressionAttributeValues) > 0 {
					params.ExpressionAttributeValues = conditionExpressionSet[0].ExpressionAttributeValues
				}
			}
		}

		// record params payload
		d.LastExecuteParamsPayload = "RemoveItemAttribute = " + params.String()

		// execute dynamodb service
		var err error

		subTrace := trace.NewSubSegment("RemoveItemAttribute_Do")
		defer subTrace.Close()

		// create timeout context
		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			_, err = d.do_UpdateItem(params, ctx)
		} else {
			_, err = d.do_UpdateItem(params, subTrace.Ctx)
		}

		if err != nil {
			ddbErr = d.handleError(err, "DynamoDB RemoveItemAttribute Failed: (UpdateItem to RemoveItemAttribute)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		} else {
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":        d.TableName,
			"PK":               pkValue,
			"SK":               skValue,
			"RemoveExpression": removeExpression,
		},
	})

	// remove item attribute successful
	return ddbErr
}

func (d *DynamoDB) removeItemAttributeNormal(pkValue string, skValue string, removeExpression string, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) (ddbErr *DynamoDBError) {
	if d.cn == nil {
		return d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// validate input parameters
	if util.LenTrim(d.PKName) <= 0 {
		return d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "PK Name is Required"))
	}

	if util.LenTrim(pkValue) <= 0 {
		return d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "PK Value is Required"))
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			return d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "SK Name is Required"))
		}
	}

	if util.LenTrim(removeExpression) <= 0 {
		return d.handleError(errors.New("DynamoDB RemoveItemAttribute Failed: " + "RemoveExpression is Required"))
	}

	// define key
	m := make(map[string]*dynamodb.AttributeValue)

	m[d.PKName] = &dynamodb.AttributeValue{S: aws.String(pkValue)}

	if util.LenTrim(skValue) > 0 {
		m[d.SKName] = &dynamodb.AttributeValue{S: aws.String(skValue)}
	}

	// build update item input params
	params := &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.TableName),
		Key:              m,
		UpdateExpression: aws.String(removeExpression),
		ReturnValues:     aws.String(dynamodb.ReturnValueAllNew),
	}

	if len(conditionExpressionSet) > 0 && conditionExpressionSet[0] != nil {
		conditionExpression := conditionExpressionSet[0].ConditionExpression

		if util.LenTrim(conditionExpression) > 0 {
			params.ConditionExpression = aws.String(conditionExpression)

			if conditionExpressionSet[0].ExpressionAttributeValues != nil && len(conditionExpressionSet[0].ExpressionAttributeValues) > 0 {
				params.ExpressionAttributeValues = conditionExpressionSet[0].ExpressionAttributeValues
			}
		}
	}

	// record params payload
	d.LastExecuteParamsPayload = "RemoveItemAttribute = " + params.String()

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
		ddbErr = d.handleError(err, "DynamoDB RemoveItemAttribute Failed: (UpdateItem to RemoveItemAttribute)")
	} else {
		ddbErr = nil
	}

	// remove item attribute successful
	return ddbErr
}

// RemoveItemAttributeWithRetry handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) RemoveItemAttributeWithRetry(maxRetries uint, pkValue string, skValue string, removeExpression string, timeOutDuration *time.Duration, conditionExpressionSet ...*DynamoDBConditionExpressionSet) *DynamoDBError {
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 10 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	} else if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}

	if err := d.RemoveItemAttribute(pkValue, skValue, removeExpression, util.DurationPtr(timeout), conditionExpressionSet...); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("RemoveItemAttributeWithRetry Failed: " + err.ErrorMessage)
				return d.RemoveItemAttributeWithRetry(maxRetries-1, pkValue, skValue, removeExpression, util.DurationPtr(timeout), conditionExpressionSet...)
			} else {
				if err.SuppressError {
					log.Println("RemoveItemAttributeWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil
				} else {
					return &DynamoDBError{
						ErrorMessage:      "RemoveItemAttributeWithRetry Failed: " + err.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("RemoveItemAttributeWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil
			} else {
				return &DynamoDBError{
					ErrorMessage:      "RemoveItemAttributeWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		return nil
	}
}

// =====================================================================================================================
// DeleteItem Functions
// =====================================================================================================================

// DeleteItem will delete an existing item from dynamodb table, using primary key values (PK and SK)
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//
//	pkValue = required, value of partition key to seek
//	skValue = optional, value of sort key to seek; set to blank if value not provided
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
func (d *DynamoDB) DeleteItem(pkValue string, skValue string, timeOutDuration *time.Duration) (ddbErr *DynamoDBError) {
	if xray.XRayServiceOn() {
		return d.deleteItemWithTrace(pkValue, skValue, timeOutDuration)
	} else {
		return d.deleteItemNormal(pkValue, skValue, timeOutDuration)
	}
}

func (d *DynamoDB) deleteItemWithTrace(pkValue string, skValue string, timeOutDuration *time.Duration) (ddbErr *DynamoDBError) {
	trace := xray.NewSegment("DynamoDB-DeleteItem", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("DynamoDB Connection is Required"))
		return ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return ddbErr
	}

	if util.LenTrim(d.PKName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB DeleteItem Failed: " + "PK Name is Required"))
		return ddbErr
	}

	if util.LenTrim(pkValue) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB DeleteItem Failed: " + "PK Value is Required"))
		return ddbErr
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			ddbErr = d.handleError(errors.New("DynamoDB DeleteItem Failed: " + "SK Name is Required"))
			return ddbErr
		}
	}

	trace.Capture("DeleteItem", func() error {
		m := make(map[string]*dynamodb.AttributeValue)

		m[d.PKName] = &dynamodb.AttributeValue{S: aws.String(pkValue)}

		if util.LenTrim(skValue) > 0 {
			m[d.SKName] = &dynamodb.AttributeValue{S: aws.String(skValue)}
		}

		params := &dynamodb.DeleteItemInput{
			TableName: aws.String(d.TableName),
			Key:       m,
		}

		// record params payload
		d.LastExecuteParamsPayload = "DeleteItem = " + params.String()

		var err error

		subTrace := trace.NewSubSegment("DeleteItem_Do")
		defer subTrace.Close()

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			_, err = d.do_DeleteItem(params, ctx)
		} else {
			_, err = d.do_DeleteItem(params, subTrace.Ctx)
		}

		if err != nil {
			ddbErr = d.handleError(err, "DynamoDB DeleteItem Failed: (DeleteItem)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		} else {
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName": d.TableName,
			"PK":        pkValue,
			"SK":        skValue,
		},
	})

	// delete item was successful
	return ddbErr
}

func (d *DynamoDB) deleteItemNormal(pkValue string, skValue string, timeOutDuration *time.Duration) (ddbErr *DynamoDBError) {
	if d.cn == nil {
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

	m[d.PKName] = &dynamodb.AttributeValue{S: aws.String(pkValue)}

	if util.LenTrim(skValue) > 0 {
		m[d.SKName] = &dynamodb.AttributeValue{S: aws.String(skValue)}
	}

	params := &dynamodb.DeleteItemInput{
		TableName: aws.String(d.TableName),
		Key:       m,
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
		ddbErr = d.handleError(err, "DynamoDB DeleteItem Failed: (DeleteItem)")
	} else {
		ddbErr = nil
	}

	// delete item was successful
	return ddbErr
}

// DeleteItemWithRetry handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) DeleteItemWithRetry(maxRetries uint, pkValue string, skValue string, timeOutDuration *time.Duration) *DynamoDBError {
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	if err := d.DeleteItem(pkValue, skValue, util.DurationPtr(timeout)); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("DeleteItemWithRetry Failed: " + err.ErrorMessage)
				return d.DeleteItemWithRetry(maxRetries-1, pkValue, skValue, util.DurationPtr(timeout))
			} else {
				if err.SuppressError {
					log.Println("DeleteItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil
				} else {
					return &DynamoDBError{
						ErrorMessage:      "DeleteItemWithRetry Failed: " + err.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("DeleteItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil
			} else {
				return &DynamoDBError{
					ErrorMessage:      "DeleteItemWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		return nil
	}
}

// =====================================================================================================================
// GetItem Functions
// =====================================================================================================================

// GetItem will find an existing item from dynamodb table
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// warning
//
//	projectedAttributes = if specified, must include PartitionKey (Hash key) typically "PK" as the first attribute in projected attributes
//
// parameters:
//
//	resultItemPtr = required, pointer to item object for return value to unmarshal into; if projected attributes less than struct fields, unmatched is defaulted
//		a) MUST BE STRUCT OBJECT; NEVER A SLICE
//	pkValue = required, value of partition key to seek
//	skValue = optional, value of sort key to seek; set to blank if value not provided
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//	consistentRead = optional, scan uses consistent read or eventual consistent read, default is eventual consistent read
//	projectedAttributes = optional; ATTRIBUTES ARE CASE SENSITIVE; variadic list of attribute names that this query will project into result items;
//					      attribute names must match struct field name or struct tag's json / dynamodbav tag values,
//						  if specified, must include PartitionKey (Hash key) typically "PK" as the first attribute in projected attributes
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) GetItem(resultItemPtr interface{},
	pkValue string, skValue string,
	timeOutDuration *time.Duration, consistentRead *bool, projectedAttributes ...string) (ddbErr *DynamoDBError) {

	if xray.XRayServiceOn() {
		return d.getItemWithTrace(resultItemPtr, pkValue, skValue, timeOutDuration, consistentRead, projectedAttributes...)
	} else {
		return d.getItemNormal(resultItemPtr, pkValue, skValue, timeOutDuration, consistentRead, projectedAttributes...)
	}
}

func (d *DynamoDB) getItemWithTrace(resultItemPtr interface{},
	pkValue string, skValue string,
	timeOutDuration *time.Duration, consistentRead *bool, projectedAttributes ...string) (ddbErr *DynamoDBError) {

	trace := xray.NewSegment("DynamoDB-GetItem", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("DynamoDB Connection is Required"))
		return ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return ddbErr
	}

	// validate input parameters
	if resultItemPtr == nil {
		ddbErr = d.handleError(errors.New("DynamoDB GetItem Failed: " + "ResultItemPtr Must Initialize First"))
		return ddbErr
	}

	if util.LenTrim(d.PKName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB GetItem Failed: " + "PK Name is Required"))
		return ddbErr
	}

	if util.LenTrim(pkValue) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB GetItem Failed: " + "PK Value is Required"))
		return ddbErr
	}

	if util.LenTrim(skValue) > 0 {
		if util.LenTrim(d.SKName) <= 0 {
			ddbErr = d.handleError(errors.New("DynamoDB GetItem Failed: " + "SK Name is Required"))
			return ddbErr
		}
	}

	trace.Capture("GetItem", func() error {
		// define key filter
		m := make(map[string]*dynamodb.AttributeValue)

		m[d.PKName] = &dynamodb.AttributeValue{S: aws.String(pkValue)}

		if util.LenTrim(skValue) > 0 {
			m[d.SKName] = &dynamodb.AttributeValue{S: aws.String(skValue)}
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
				ddbErr = d.handleError(err, "DynamoDB GetItem Failed: (GetItem)")
				return fmt.Errorf(ddbErr.ErrorMessage)
			}
		}

		// set params
		params := &dynamodb.GetItemInput{
			TableName: aws.String(d.TableName),
			Key:       m,
		}

		if projSet {
			params.ProjectionExpression = expr.Projection()
			params.ExpressionAttributeNames = expr.Names()
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

		subTrace := trace.NewSubSegment("GetItem_Do")
		defer subTrace.Close()

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			result, err = d.do_GetItem(params, ctx)
		} else {
			result, err = d.do_GetItem(params, subTrace.Ctx)
		}

		// evaluate result
		if err != nil {
			ddbErr = d.handleError(err, "DynamoDB GetItem Failed: (GetItem)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		}

		if result == nil {
			ddbErr = d.handleError(errors.New("DynamoDB GetItem Failed: " + "Result Object Nil"))
			return fmt.Errorf(ddbErr.ErrorMessage)
		}

		if err = dynamodbattribute.UnmarshalMap(result.Item, resultItemPtr); err != nil {
			ddbErr = d.handleError(err, "DynamoDB GetItem Failed: (Unmarshal)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		} else {
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName": d.TableName,
			"PK":        pkValue,
			"SK":        skValue,
		},
	})

	// get item was successful
	return ddbErr
}

func (d *DynamoDB) getItemNormal(resultItemPtr interface{},
	pkValue string, skValue string,
	timeOutDuration *time.Duration, consistentRead *bool, projectedAttributes ...string) (ddbErr *DynamoDBError) {
	if d.cn == nil {
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

	m[d.PKName] = &dynamodb.AttributeValue{S: aws.String(pkValue)}

	if util.LenTrim(skValue) > 0 {
		m[d.SKName] = &dynamodb.AttributeValue{S: aws.String(skValue)}
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
		Key:       m,
	}

	if projSet {
		params.ProjectionExpression = expr.Projection()
		params.ExpressionAttributeNames = expr.Names()
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
		ddbErr = d.handleError(err, "DynamoDB GetItem Failed: (Unmarshal)")
	} else {
		ddbErr = nil
	}

	// get item was successful
	return ddbErr
}

// GetItemWithRetry handles dynamodb retries in case action temporarily fails
//
// warning
//
//	projectedAttributes = if specified, must include PartitionKey (Hash key) typically "PK" as the first attribute in projected attributes
func (d *DynamoDB) GetItemWithRetry(maxRetries uint,
	resultItemPtr interface{}, pkValue string, skValue string,
	timeOutDuration *time.Duration, consistentRead *bool, projectedAttributes ...string) *DynamoDBError {
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	if err := d.GetItem(resultItemPtr, pkValue, skValue, util.DurationPtr(timeout), consistentRead, projectedAttributes...); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("GetItemWithRetry Failed: " + err.ErrorMessage)
				return d.GetItemWithRetry(maxRetries-1, resultItemPtr, pkValue, skValue, util.DurationPtr(timeout), consistentRead, projectedAttributes...)
			} else {
				if err.SuppressError {
					log.Println("GetItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil
				} else {
					return &DynamoDBError{
						ErrorMessage:      "GetItemWithRetry Failed: " + err.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("GetItemWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil
			} else {
				return &DynamoDBError{
					ErrorMessage:      "GetItemWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		return nil
	}
}

// =====================================================================================================================
// QueryPaginationData Functions
// =====================================================================================================================

// QueryPaginationDataWithRetry returns slice of ExclusiveStartKeys,
// with first element always a nil to represent no exclusiveStartKey,
// and each subsequent element starts from page 2 with its own exclusiveStartKey.
//
// if slice is nil or zero element, then it also indicates single page,
// same as if slice is single element with nil indicating single page.
//
// Caller can use this info to pre-build the pagination buttons, so that clicking page 1 simply query using no exclusiveStartKey,
// where as query page 2 uses the exclusiveStartKey from element 1 of the slice, and so on.
func (d *DynamoDB) QueryPaginationDataWithRetry(
	maxRetries uint,
	timeOutDuration *time.Duration,
	indexName *string,
	itemsPerPage int64,
	keyConditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue) (paginationData []map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {

	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	if paginationData, ddbErr = d.queryPaginationDataWrapper(util.DurationPtr(timeout), indexName, itemsPerPage, keyConditionExpression, expressionAttributeNames, expressionAttributeValues); ddbErr != nil {
		// has error
		if maxRetries > 0 {
			if ddbErr.AllowRetry {
				if ddbErr.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("QueryPaginationDataWithRetry Failed: " + ddbErr.ErrorMessage)
				return d.QueryPaginationDataWithRetry(maxRetries-1, util.DurationPtr(timeout), indexName, itemsPerPage, keyConditionExpression, expressionAttributeNames, expressionAttributeValues)
			} else {
				if ddbErr.SuppressError {
					log.Println("QueryPaginationDataWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil, nil
				} else {
					return nil, &DynamoDBError{
						ErrorMessage:      "QueryPaginationDataWithRetry Failed: " + ddbErr.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if ddbErr.SuppressError {
				log.Println("QueryPaginationDataWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil, nil
			} else {
				return nil, &DynamoDBError{
					ErrorMessage:      "QueryPaginationDataWithRetry Failed: (MaxRetries = 0) " + ddbErr.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		if paginationData == nil {
			paginationData = make([]map[string]*dynamodb.AttributeValue, 1)
		} else {
			paginationData = append([]map[string]*dynamodb.AttributeValue{nil}, paginationData...)
		}

		return paginationData, nil
	}
}

func (d *DynamoDB) queryPaginationDataWrapper(
	timeOutDuration *time.Duration,
	indexName *string,
	itemsPerPage int64,
	keyConditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue) (paginationData []map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {

	if xray.XRayServiceOn() {
		return d.queryPaginationDataWithTrace(timeOutDuration, indexName, itemsPerPage, keyConditionExpression, expressionAttributeNames, expressionAttributeValues)
	} else {
		return d.queryPaginationDataNormal(timeOutDuration, indexName, itemsPerPage, keyConditionExpression, expressionAttributeNames, expressionAttributeValues)
	}
}

func (d *DynamoDB) queryPaginationDataWithTrace(
	timeOutDuration *time.Duration,
	indexName *string,
	itemsPerPage int64,
	keyConditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue) (paginationData []map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {

	trace := xray.NewSegment("DynamoDB-QueryPaginationDataWithTrace", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("QueryPaginationDataWithTrace Failed: DynamoDB Connection is Required"))
		return nil, ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("QueryPaginationDataWithTrace Failed: DynamoDB Table Name is Required"))
		return nil, ddbErr
	}

	// validate additional input parameters
	if util.LenTrim(keyConditionExpression) <= 0 {
		ddbErr = d.handleError(errors.New("QueryPaginationDataWithTrace Failed: KeyConditionExpress is Required"))
		return nil, ddbErr
	}

	if expressionAttributeValues == nil {
		ddbErr = d.handleError(errors.New("QueryPaginationDataWithTrace Failed: ExpressionAttributeValues is Required"))
		return nil, ddbErr
	}

	trace.Capture("QueryPaginationDataWithTrace", func() error {
		// compose filter expression and projection if applicable
		expr, err := expression.NewBuilder().WithProjection(expression.NamesList(expression.Name("PK"))).Build()

		if err != nil {
			ddbErr = d.handleError(err, "QueryPaginationDataWithTrace Failed: (Filter/Projection Expression Build)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		}

		// build query input params
		params := &dynamodb.QueryInput{
			TableName:                 aws.String(d.TableName),
			KeyConditionExpression:    aws.String(keyConditionExpression),
			ExpressionAttributeValues: expressionAttributeValues,
		}

		if expressionAttributeNames != nil {
			params.ExpressionAttributeNames = expressionAttributeNames
		}

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

		params.ProjectionExpression = expr.Projection()

		if params.ExpressionAttributeNames == nil {
			params.ExpressionAttributeNames = expr.Names()
		} else {
			for k1, v1 := range expr.Names() {
				params.ExpressionAttributeNames[k1] = v1
			}
		}

		if indexName != nil && util.LenTrim(*indexName) > 0 {
			params.IndexName = indexName
		}

		params.Limit = aws.Int64(itemsPerPage)

		// record params payload
		d.LastExecuteParamsPayload = "QueryPaginationDataWithTrace = " + params.String()

		subTrace := trace.NewSubSegment("QueryPaginationDataWithTrace_Do")
		defer subTrace.Close()

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			paginationData, err = d.do_Query_Pagination_Data(params, ctx)
		} else {
			paginationData, err = d.do_Query_Pagination_Data(params, subTrace.Ctx)
		}

		if err != nil {
			ddbErr = d.handleError(err, "QueryPaginationDataWithTrace Failed: (QueryPaginationDataWithTrace)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		}

		return nil

	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":                 d.TableName,
			"IndexName":                 aws.StringValue(indexName),
			"KeyConditionExpression":    keyConditionExpression,
			"ExpressionAttributeNames":  expressionAttributeNames,
			"ExpressionAttributeValues": expressionAttributeValues,
		},
	})

	// query items successful
	return paginationData, ddbErr
}

func (d *DynamoDB) queryPaginationDataNormal(
	timeOutDuration *time.Duration,
	indexName *string,
	itemsPerPage int64,
	keyConditionExpression string,
	expressionAttributeNames map[string]*string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue) (paginationData []map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {

	if d.cn == nil {
		return nil, d.handleError(errors.New("QueryPaginationDataNormal Failed: DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return nil, d.handleError(errors.New("QueryPaginationDataNormal Failed: DynamoDB Table Name is Required"))
	}

	// validate additional input parameters
	if util.LenTrim(keyConditionExpression) <= 0 {
		return nil, d.handleError(errors.New("QueryPaginationDataNormal Failed: KeyConditionExpress is Required"))
	}

	if expressionAttributeValues == nil {
		return nil, d.handleError(errors.New("QueryPaginationDataNormal Failed: ExpressionAttributeValues is Required"))
	}

	// compose filter expression and projection if applicable
	expr, err := expression.NewBuilder().WithProjection(expression.NamesList(expression.Name("PK"))).Build()

	if err != nil {
		return nil, d.handleError(err, "QueryPaginationDataNormal Failed: (Filter/Projection Expression Build)")
	}

	// build query input params
	params := &dynamodb.QueryInput{
		TableName:                 aws.String(d.TableName),
		KeyConditionExpression:    aws.String(keyConditionExpression),
		ExpressionAttributeValues: expressionAttributeValues,
	}

	if expressionAttributeNames != nil {
		params.ExpressionAttributeNames = expressionAttributeNames
	}

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

	params.ProjectionExpression = expr.Projection()

	if params.ExpressionAttributeNames == nil {
		params.ExpressionAttributeNames = expr.Names()
	} else {
		for k1, v1 := range expr.Names() {
			params.ExpressionAttributeNames[k1] = v1
		}
	}

	if indexName != nil && util.LenTrim(*indexName) > 0 {
		params.IndexName = indexName
	}

	params.Limit = aws.Int64(itemsPerPage)

	// record params payload
	d.LastExecuteParamsPayload = "QueryPaginationDataNormal = " + params.String()

	if timeOutDuration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), *timeOutDuration)
		defer cancel()
		paginationData, err = d.do_Query_Pagination_Data(params, ctx)
	} else {
		paginationData, err = d.do_Query_Pagination_Data(params)
	}

	if err != nil {
		return nil, d.handleError(err, "QueryPaginationDataNormal Failed: (QueryPaginationDataNormal)")
	}

	if paginationData == nil {
		return nil, d.handleError(err, "QueryPaginationDataNormal Failed: (QueryPaginationDataNormal)")
	}

	// query pagination data successful
	return paginationData, nil
}

// =====================================================================================================================
// QueryItems Functions
// =====================================================================================================================

// QueryItems will query dynamodb items in given table using primary key (PK, SK for example), or one of Global/Local Secondary Keys (indexName must be defined if using GSI)
// To query against non-key attributes, use Scan (bad for performance however)
// QueryItems requires using Key attributes, and limited to TWO key attributes in condition maximum;
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search without GSI/LSI, MUST use PK and SK together or error will trigger
//
// warning
//
//	projectedAttributes = if specified, must include PartitionKey (Hash key) typically "PK" as the first attribute in projected attributes
//
// parameters:
//
//	resultItemsPtr = required, pointer to items list struct to contain queried result; i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//	consistentRead = optional, scan uses consistent read or eventual consistent read, default is eventual consistent read
//	indexName = optional, global secondary index or local secondary index name to help in query operation
//	pageLimit = optional, scan page limit if set, this limits number of items examined per page during scan operation, allowing scan to work better for RCU
//	pagedQuery = optional, indicates if query is page based or not; if true, query will be performed via pages, this helps overcome 1 MB limit of each query result
//	pagedQueryPageCountLimit = optional, indicates how many pages to query during paged query action
//	exclusiveStartKey = optional, if using pagedQuery and starting the query from prior results
//
//	keyConditionExpression = required, ATTRIBUTES ARE CASE SENSITIVE; either the primary key (PK SK for example) or global secondary index (SK Data for example) or another secondary index (secondary index must be named)
//		Usage Syntax:
//			1) Max 2 Attribute Fields
//			2) First Field must be Partition Key (Must Evaluate to True or False)
//				a) = ONLY
//			3) Second Field is Sort Key (May Evaluate to True or False or Range)
//				a) =, <, <=, >, >=, BETWEEN, begins_with()
//			4) Combine Two Fields with AND
//			5) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//				a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//			6) Example
//				a) partitionKeyName = :partitionKeyVal
//				b) partitionKeyName = :partitionKeyVal AND sortKeyName = :sortKeyVal
//				c) #yr = :year
//			7) If Using GSI / Local Index
//				a) When Using, Must Specify the IndexName
//				b) First Field is the GSI's Partition Key, such as SK (Evals to True/False), While Second Field is the GSI's SortKey (Range)
//
//	expressionAttributeNames = optional, ATTRIBUTES ARE CASE SENSITIVE; set nil if not used, must define for attribute names that are reserved keywords such as year, data etc. using #xyz
//		Usage Syntax:
//			1) map[string]*string: where string is the #xyz, and *string is the original xyz attribute name
//				a) map[string]*string { "#xyz": aws.String("Xyz"), }
//			2) Add to Map
//				a) m := make(map[string]*string)
//				b) m["#xyz"] = aws.String("Xyz")
//
//	expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//		Usage Syntax:
//			1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//				a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//			2) Add to Map
//				a) m := make(map[string]*dynamodb.AttributeValue)
//				b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//			3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//				a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//				b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
//
//	filterConditionExpression = optional; ATTRIBUTES ARE CASE SENSITIVE; once query on key conditions returned, this filter condition further restricts return data before output to caller;
//		Usage Syntax:
//			1) &expression.Name(xyz).Equals(expression.Value(abc))
//			2) &expression.Name(xyz).Equals(expression.Value(abc)).And(...)
//
//	projectedAttributes = optional; ATTRIBUTES ARE CASE SENSITIVE; variadic list of attribute names that this query will project into result items;
//					      attribute names must match struct field name or struct tag's json / dynamodbav tag values
//
// Return Values:
//
//	prevEvalKey = if paged query, the last evaluate key returned, to be used in subsequent query via exclusiveStartKey; otherwise always nil is returned
//				  prevEvalkey map is set into exclusiveStartKey field if more data to load
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
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

	if xray.XRayServiceOn() {
		return d.queryItemsWithTrace(resultItemsPtr, timeOutDuration, consistentRead, indexName, pageLimit, pagedQuery, pagedQueryPageCountLimit, exclusiveStartKey,
			keyConditionExpression, expressionAttributeNames, expressionAttributeValues, filterConditionExpression, projectedAttributes...)
	} else {
		return d.queryItemsNormal(resultItemsPtr, timeOutDuration, consistentRead, indexName, pageLimit, pagedQuery, pagedQueryPageCountLimit, exclusiveStartKey,
			keyConditionExpression, expressionAttributeNames, expressionAttributeValues, filterConditionExpression, projectedAttributes...)
	}
}

func (d *DynamoDB) queryItemsWithTrace(resultItemsPtr interface{},
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

	trace := xray.NewSegment("DynamoDB-QueryItems", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("DynamoDB Connection is Required"))
		return nil, ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return nil, ddbErr
	}

	// result items pointer must be set
	if resultItemsPtr == nil {
		ddbErr = d.handleError(errors.New("DynamoDB QueryItems Failed: " + "ResultItems is Nil"))
		return nil, ddbErr
	}

	// validate additional input parameters
	if util.LenTrim(keyConditionExpression) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB QueryItems Failed: " + "KeyConditionExpress is Required"))
		return nil, ddbErr
	}

	if expressionAttributeValues == nil {
		ddbErr = d.handleError(errors.New("DynamoDB QueryItems Failed: " + "ExpressionAttributeValues is Required"))
		return nil, ddbErr
	}

	// execute dynamodb service
	var result *dynamodb.QueryOutput

	trace.Capture("QueryItems", func() error {
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
			projSet = false
		} else if projSet {
			expr, err = expression.NewBuilder().WithProjection(proj).Build()
			filterSet = false
			projSet = true
		}

		if err != nil {
			ddbErr = d.handleError(err, "DynamoDB QueryItems Failed (Filter/Projection Expression Build)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		}

		// build query input params
		params := &dynamodb.QueryInput{
			TableName:                 aws.String(d.TableName),
			KeyConditionExpression:    aws.String(keyConditionExpression),
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

			if params.ExpressionAttributeNames == nil {
				params.ExpressionAttributeNames = expr.Names()
			} else {
				for k1, v1 := range expr.Names() {
					params.ExpressionAttributeNames[k1] = v1
				}
			}
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

		if indexName != nil && util.LenTrim(*indexName) > 0 {
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

		subTrace := trace.NewSubSegment("QueryItems_Do")
		defer subTrace.Close()

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			result, err = d.do_Query(params, pagedQuery, pagedQueryPageCountLimit, ctx)
		} else {
			result, err = d.do_Query(params, pagedQuery, pagedQueryPageCountLimit, subTrace.Ctx)
		}

		if err != nil {
			ddbErr = d.handleError(err, "DynamoDB QueryItems Failed: (QueryItems)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		}

		if result == nil {
			return nil
		}

		// unmarshal result items to target object map
		if err = dynamodbattribute.UnmarshalListOfMaps(result.Items, resultItemsPtr); err != nil {
			ddbErr = d.handleError(err, "Dynamo QueryItems Failed: (Unmarshal Result Items)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		} else {
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":                 d.TableName,
			"IndexName":                 aws.StringValue(indexName),
			"ExclusiveStartKey":         exclusiveStartKey,
			"KeyConditionExpression":    keyConditionExpression,
			"ExpressionAttributeNames":  expressionAttributeNames,
			"ExpressionAttributeValues": expressionAttributeValues,
			"FilterConditionExpression": filterConditionExpression,
		},
	})

	// query items successful
	if result != nil {
		return result.LastEvaluatedKey, ddbErr
	} else {
		return nil, ddbErr
	}
}

func (d *DynamoDB) queryItemsNormal(resultItemsPtr interface{},
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

	// execute dynamodb service
	var result *dynamodb.QueryOutput

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
		projSet = false
	} else if projSet {
		expr, err = expression.NewBuilder().WithProjection(proj).Build()
		filterSet = false
		projSet = true
	}

	if err != nil {
		return nil, d.handleError(err, "DynamoDB QueryItems Failed (Filter/Projection Expression Build)")
	}

	// build query input params
	params := &dynamodb.QueryInput{
		TableName:                 aws.String(d.TableName),
		KeyConditionExpression:    aws.String(keyConditionExpression),
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

		if params.ExpressionAttributeNames == nil {
			params.ExpressionAttributeNames = expr.Names()
		} else {
			for k1, v1 := range expr.Names() {
				params.ExpressionAttributeNames[k1] = v1
			}
		}
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

	if indexName != nil && util.LenTrim(*indexName) > 0 {
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

	if result == nil {
		return nil, d.handleError(err, "DynamoDB QueryItems Failed: (QueryItems)")
	}

	// unmarshal result items to target object map
	if err = dynamodbattribute.UnmarshalListOfMaps(result.Items, resultItemsPtr); err != nil {
		ddbErr = d.handleError(err, "Dynamo QueryItems Failed: (Unmarshal Result Items)")
	} else {
		ddbErr = nil
	}

	// query items successful
	return result.LastEvaluatedKey, ddbErr
}

// QueryItemsWithRetry handles dynamodb retries in case action temporarily fails
//
// warning
//
//	projectedAttributes = if specified, must include PartitionKey (Hash key) typically "PK" as the first attribute in projected attributes
func (d *DynamoDB) QueryItemsWithRetry(maxRetries uint,
	resultItemsPtr interface{},
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
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	if prevEvalKey, ddbErr = d.QueryItems(resultItemsPtr, util.DurationPtr(timeout), consistentRead, indexName, pageLimit,
		pagedQuery, pagedQueryPageCountLimit, exclusiveStartKey, keyConditionExpression,
		expressionAttributeNames, expressionAttributeValues, filterConditionExpression, projectedAttributes...); ddbErr != nil {
		// has error
		if maxRetries > 0 {
			if ddbErr.AllowRetry {
				if ddbErr.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("QueryItemsWithRetry Failed: " + ddbErr.ErrorMessage)
				return d.QueryItemsWithRetry(maxRetries-1,
					resultItemsPtr, util.DurationPtr(timeout), consistentRead, indexName, pageLimit,
					pagedQuery, pagedQueryPageCountLimit, exclusiveStartKey, keyConditionExpression,
					expressionAttributeNames, expressionAttributeValues, filterConditionExpression, projectedAttributes...)
			} else {
				if ddbErr.SuppressError {
					log.Println("QueryItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil, nil
				} else {
					return nil, &DynamoDBError{
						ErrorMessage:      "QueryItemsWithRetry Failed: " + ddbErr.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if ddbErr.SuppressError {
				log.Println("QueryItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil, nil
			} else {
				return nil, &DynamoDBError{
					ErrorMessage:      "QueryItemsWithRetry Failed: (MaxRetries = 0) " + ddbErr.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		return prevEvalKey, nil
	}
}

// =====================================================================================================================
// QueryPagedItems Functions
// =====================================================================================================================

// QueryPagedItemsWithRetry will query dynamodb items in given table using primary key (PK, SK for example),
// or one of Global/Local Secondary Keys (indexName must be defined if using GSI)
//
// To query against non-key attributes, use Scan (bad for performance however)
// QueryItems requires using Key attributes, and limited to TWO key attributes in condition maximum;
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search without GSI/LSI, MUST use PK and SK together or error will trigger
//
// parameters:
//
//	pagedSlicePtr = required, identifies the actual slice pointer for use during paged query
//					(this parameter is not the output of result, actual result is returned via return variable returnItemsList)
//	resultSlicePtr = required, pointer to working items list struct to contain queried result;
//					 i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted;
//					 (this parameter is not the output of result, actual result is returned via return variable returnItemsList)
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//	consistentRead = (always set to false for paged query internally)
//	indexName = optional, global secondary index or local secondary index name to help in query operation
//	pageLimit = (always set to 100 internally)
//	pagedQuery = (always set to true internally)
//	pagedQueryPageCountLimit = (always set to 25 internally)
//	exclusiveStartKey = (set internally by the paged query loop if any exists)
//	keyConditionExpression = required, ATTRIBUTES ARE CASE SENSITIVE; either the primary key (PK SK for example) or global secondary index (SK Data for example) or another secondary index (secondary index must be named)
//		Usage Syntax:
//			1) Max 2 Attribute Fields
//			2) First Field must be Partition Key (Must Evaluate to True or False)
//				a) = ONLY
//			3) Second Field is Sort Key (May Evaluate to True or False or Range)
//				a) =, <, <=, >, >=, BETWEEN, begins_with()
//			4) Combine Two Fields with AND
//			5) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//				a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//			6) Example
//				a) partitionKeyName = :partitionKeyVal
//				b) partitionKeyName = :partitionKeyVal AND sortKeyName = :sortKeyVal
//				c) #yr = :year
//			7) If Using GSI / Local Index
//				a) When Using, Must Specify the IndexName
//				b) First Field is the GSI's Partition Key, such as SK (Evals to True/False), While Second Field is the GSI's SortKey (Range)
//	expressionAttributeNames = (always nil internally, not used in paged query)
//	expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//		Usage Syntax:
//			1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//				a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//			2) Add to Map
//				a) m := make(map[string]*dynamodb.AttributeValue)
//				b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//			3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//				a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//				b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
//	filterConditionExpression = optional; ATTRIBUTES ARE CASE SENSITIVE; once query on key conditions returned, this filter condition further restricts return data before output to caller;
//		Usage Syntax:
//			1) &expression.Name(xyz).Equals(expression.Value(abc))
//			2) &expression.Name(xyz).Equals(expression.Value(abc)).And(...)
//	projectedAttributes = (always nil internally for paged query)
//
// Return Values:
//
//	returnItemsList = interface{} of return slice, use assert to cast to target type
//	err = error info if error is encountered
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) QueryPagedItemsWithRetry(maxRetries uint,
	pagedSlicePtr interface{},
	resultSlicePtr interface{},
	timeOutDuration *time.Duration,
	indexName string,
	keyConditionExpression string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue,
	filterConditionExpression *expression.ConditionBuilder) (returnItemsList interface{}, err error) {

	if pagedSlicePtr == nil {
		return nil, fmt.Errorf("PagedSlicePtr Identifies Working Slice Pointer During Query and is Required")
	} else {
		if valPaged := reflect.ValueOf(pagedSlicePtr); valPaged.Kind() != reflect.Ptr {
			return nil, fmt.Errorf("PagedSlicePtr Expected To Be Slice Pointer (Not Ptr)")
		} else if valPaged.Elem().Kind() != reflect.Slice {
			return nil, fmt.Errorf("PagedSlicePtr Expected To Be Slice Pointer (Not Slice)")
		}
	}

	if resultSlicePtr == nil {
		return nil, fmt.Errorf("ResultSlicePtr Contains Query Result in Slice Pointer and is Required")
	} else {
		if valResult := reflect.ValueOf(resultSlicePtr); valResult.Kind() != reflect.Ptr {
			return nil, fmt.Errorf("ResultSlicePtr Expected To Be Slice Pointer (Not Ptr)")
		} else if valResult.Elem().Kind() != reflect.Slice {
			return nil, fmt.Errorf("ResultSlicePtr Expected To Be Slice Pointer (Not Slice)")
		}
	}

	var prevEvalKey map[string]*dynamodb.AttributeValue
	prevEvalKey = nil

	var e *DynamoDBError

	pageLimit := int64(250)              // changed from 100 to 250, since typical record is 4k or less and 250 is about 1mb or less
	pagedQueryPageCountLimit := int64(1) // changed to 1 from 25

	var indexNamePtr *string

	if util.LenTrim(indexName) > 0 {
		indexNamePtr = aws.String(indexName)
	} else {
		indexNamePtr = nil
	}

	var pagedSliceTemp reflect.Value
	// Initialize pagedSliceTemp to avoid encountering the 'call of unknown method on zero Value' issue with subsequent reflect.AppendSlice calls.
	if reflect.TypeOf(resultSlicePtr).Kind() == reflect.Ptr {
		pagedSliceTemp = reflect.ValueOf(resultSlicePtr).Elem()
	} else {
		pagedSliceTemp = reflect.ValueOf(resultSlicePtr)
	}

	for {
		// We create a new `pagedSlicePtr` variable for each `for` loop iteration instead of reusing the same one
		// because it is a pointer. When using `reflect.AppendSlice`, only a pointer struct is copied into the slice.
		// Each iteration of the `dynamodbattribute.UnmarshalListOfMaps` method modifies the content pointed to by this pointer,
		// resulting in the slice containing data from only the last iteration.
		originalSliceValue := reflect.ValueOf(pagedSlicePtr).Elem()
		newPagedSlice := reflect.MakeSlice(originalSliceValue.Type(), 0, 0)
		newPagedSlicePtr := reflect.New(newPagedSlice.Type()).Interface()

		// each time queried, we process up to 25 pages with each page up to 100 items,
		// if there are more data, the prevEvalKey will contain value,
		// so the for loop will continue query again until prevEvalKey is nil,
		// this method will retrieve all filtered data from data store, but may take longer time if there are more data
		if prevEvalKey, e = d.QueryItemsWithRetry(maxRetries, newPagedSlicePtr, timeOutDuration, nil, indexNamePtr,
			aws.Int64(pageLimit), true, aws.Int64(pagedQueryPageCountLimit), prevEvalKey,
			keyConditionExpression, nil, expressionAttributeValues,
			filterConditionExpression); e != nil {
			// error
			return nil, fmt.Errorf("QueryPagedItemsWithRetry Failed: %s", e)
		} else {
			// success
			//var valTarget reflect.Value

			//if reflect.TypeOf(resultSlicePtr).Kind() == reflect.Ptr {
			//	valTarget = reflect.ValueOf(resultSlicePtr).Elem()
			//} else {
			//	valTarget = reflect.ValueOf(resultSlicePtr)
			//}

			//val := reflect.AppendSlice(valTarget, reflect.ValueOf(pagedSlicePtr).Elem())
			//resultSlicePtr = val.Interface()
			pagedSliceTemp = reflect.AppendSlice(pagedSliceTemp, reflect.ValueOf(newPagedSlicePtr).Elem())

			if prevEvalKey == nil {
				break
			}

			if len(prevEvalKey) == 0 {
				break
			}
		}
	}

	resultSlicePtr = pagedSliceTemp.Interface()

	return resultSlicePtr, nil
}

// =====================================================================================================================
// QueryPerPageItems Functions
// =====================================================================================================================

// QueryPerPageItemsWithRetry will query dynamodb items in given table using primary key (PK, SK for example),
// or one of Global/Local Secondary Keys (indexName must be defined if using GSI);
//
// *** This Query is used for pagination where each query returns a specified set of items, along with the prevEvalKey,
// in the subsequent paged queries using this method, passing prevEvalKey to the exclusiveStartKey parameter will return
// next page of items from the exclusiveStartKey position ***
//
// To query against non-key attributes, use Scan (bad for performance however)
// QueryItems requires using Key attributes, and limited to TWO key attributes in condition maximum;
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search without GSI/LSI, MUST use PK and SK together or error will trigger
//
// parameters:
//
//	maxRetries = number of retries to attempt
//	itemsPerPage = query per page items count, if < 0 = 10; if > 500 = 500; defaults to 10 if 0
//	exclusiveStartKey = if query is continuation from prior pagination, then the prior query's prevEvalKey is passed into this field
//	pagedSlicePtr = required, identifies the actual slice pointer for use during paged query
//					i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted;
//					(this parameter is not the output of result, actual result is returned via return variable returnItemsList)
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//	indexName = optional, global secondary index or local secondary index name to help in query operation
//	keyConditionExpression = required, ATTRIBUTES ARE CASE SENSITIVE; either the primary key (PK SK for example) or global secondary index (SK Data for example) or another secondary index (secondary index must be named)
//		Usage Syntax:
//			1) Max 2 Attribute Fields
//			2) First Field must be Partition Key (Must Evaluate to True or False)
//				a) = ONLY
//			3) Second Field is Sort Key (May Evaluate to True or False or Range)
//				a) =, <, <=, >, >=, BETWEEN, begins_with()
//			4) Combine Two Fields with AND
//			5) When Attribute Name is Reserved Keyword, Use ExpressionAttributeNames to Define #xyz to Alias
//				a) Use the #xyz in the KeyConditionExpression such as #yr = :year (:year is Defined ExpressionAttributeValue)
//			6) Example
//				a) partitionKeyName = :partitionKeyVal
//				b) partitionKeyName = :partitionKeyVal AND sortKeyName = :sortKeyVal
//				c) #yr = :year
//			7) If Using GSI / Local Index
//				a) When Using, Must Specify the IndexName
//				b) First Field is the GSI's Partition Key, such as SK (Evals to True/False), While Second Field is the GSI's SortKey (Range)
//	expressionAttributeValues = required, ATTRIBUTES ARE CASE SENSITIVE; sets the value token and value actual to be used within the keyConditionExpression; this sets both compare token and compare value
//		Usage Syntax:
//			1) map[string]*dynamodb.AttributeValue: where string is the :xyz, and *dynamodb.AttributeValue is { S: aws.String("abc"), },
//				a) map[string]*dynamodb.AttributeValue { ":xyz" : { S: aws.String("abc"), }, ":xyy" : { N: aws.String("123"), }, }
//			2) Add to Map
//				a) m := make(map[string]*dynamodb.AttributeValue)
//				b) m[":xyz"] = &dynamodb.AttributeValue{ S: aws.String("xyz") }
//			3) Slice of Strings -> CONVERT To Slice of *dynamodb.AttributeValue = []string -> []*dynamodb.AttributeValue
//				a) av, err := dynamodbattribute.MarshalList(xyzSlice)
//				b) ExpressionAttributeValue, Use 'L' To Represent the List for av defined in 3.a above
//	filterConditionExpression = optional; ATTRIBUTES ARE CASE SENSITIVE; once query on key conditions returned, this filter condition further restricts return data before output to caller;
//		Usage Syntax:
//			1) &expression.Name(xyz).Equals(expression.Value(abc))
//			2) &expression.Name(xyz).Equals(expression.Value(abc)).And(...)
//
// Return Values:
//
//	returnItemsList = interface{} of return slice, use assert to cast to target type
//	prevEvalKey = map[string]*dynamodb.Attribute, if there are more pages, this value is then used in the subsequent query's exclusiveStartKey parameter
//	err = error info if error is encountered
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) QueryPerPageItemsWithRetry(
	maxRetries uint,
	itemsPerPage int64,
	exclusiveStartKey map[string]*dynamodb.AttributeValue,
	pagedSlicePtr interface{},
	timeOutDuration *time.Duration,
	indexName string,
	keyConditionExpression string,
	expressionAttributeValues map[string]*dynamodb.AttributeValue,
	filterConditionExpression *expression.ConditionBuilder) (returnItemsList interface{}, prevEvalKey map[string]*dynamodb.AttributeValue, err error) {

	if pagedSlicePtr == nil {
		return nil, nil, fmt.Errorf("PagedSlicePtr Identifies Working Slice Pointer During Query and is Required")
	} else {
		if valPaged := reflect.ValueOf(pagedSlicePtr); valPaged.Kind() != reflect.Ptr {
			return nil, nil, fmt.Errorf("PagedSlicePtr Expected To Be Slice Pointer (Not Ptr)")
		} else if valPaged.Elem().Kind() != reflect.Slice {
			return nil, nil, fmt.Errorf("PagedSlicePtr Expected To Be Slice Pointer (Not Slice)")
		}
	}

	var e *DynamoDBError

	if itemsPerPage <= 0 {
		itemsPerPage = 10
	} else if itemsPerPage > 500 {
		itemsPerPage = 500
	}

	var indexNamePtr *string

	if util.LenTrim(indexName) > 0 {
		indexNamePtr = aws.String(indexName)
	} else {
		indexNamePtr = nil
	}

	if prevEvalKey, e = d.QueryItemsWithRetry(maxRetries, pagedSlicePtr, timeOutDuration, nil, indexNamePtr,
		aws.Int64(itemsPerPage), true, aws.Int64(1), exclusiveStartKey,
		keyConditionExpression, nil, expressionAttributeValues,
		filterConditionExpression); e != nil {
		// error
		return nil, nil, fmt.Errorf("QueryPerPageItemsWithRetry Failed: %s", e)
	} else {
		// success
		if len(prevEvalKey) == 0 {
			prevEvalKey = nil
		}

		return reflect.ValueOf(pagedSlicePtr).Elem().Interface(), prevEvalKey, nil
	}
}

// =====================================================================================================================
// ScanItems Functions
// =====================================================================================================================

// ScanItems will scan dynamodb items in given table, project results, using filter expression
// >>> DO NOT USE SCAN IF POSSIBLE - SCAN IS NOT EFFICIENT ON RCU <<<
//
// warning
//
//	projectedAttributes = if specified, must include PartitionKey (Hash key) typically "PK" as the first attribute in projected attributes
//
// parameters:
//
//	resultItemsPtr = required, pointer to items list struct to contain queried result; i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//	consistentRead = optional, scan uses consistent read or eventual consistent read, default is eventual consistent read
//	indexName = optional, global secondary index or local secondary index name to help in scan operation
//	pageLimit = optional, scan page limit if set, this limits number of items examined per page during scan operation, allowing scan to work better for RCU
//	pagedQuery = optional, indicates if query is page based or not; if true, query will be performed via pages, this helps overcome 1 MB limit of each query result
//	pagedQueryPageCountLimit = optional, indicates how many pages to query during paged query action
//	exclusiveStartKey = optional, if using pagedQuery and starting the query from prior results
//
//	filterConditionExpression = required; ATTRIBUTES ARE CASE SENSITIVE; sets the scan filter condition;
//		Usage Syntax:
//			1) expFilter := expression.Name(xyz).Equals(expression.Value(abc))
//			2) expFilter := expression.Name(xyz).Equals(expression.Value(abc)).And(...)
//			3) Assign expFilter into filterConditionExpression
//
//	projectedAttributes = optional; ATTRIBUTES ARE CASE SENSITIVE; variadic list of attribute names that this query will project into result items;
//					      attribute names must match struct field name or struct tag's json / dynamodbav tag values
//
// Return Values:
//
//	prevEvalKey = if paged query, the last evaluate key returned, to be used in subsequent query via exclusiveStartKey; otherwise always nil is returned
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) ScanItems(resultItemsPtr interface{},
	timeOutDuration *time.Duration,
	consistentRead *bool,
	indexName *string,
	pageLimit *int64,
	pagedQuery bool,
	pagedQueryPageCountLimit *int64,
	exclusiveStartKey map[string]*dynamodb.AttributeValue,
	filterConditionExpression expression.ConditionBuilder, projectedAttributes ...string) (prevEvalKey map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {
	if xray.XRayServiceOn() {
		return d.scanItemsWithTrace(resultItemsPtr, timeOutDuration, consistentRead, indexName, pageLimit, pagedQuery, pagedQueryPageCountLimit, exclusiveStartKey, filterConditionExpression, projectedAttributes...)
	} else {
		return d.scanItemsNormal(resultItemsPtr, timeOutDuration, consistentRead, indexName, pageLimit, pagedQuery, pagedQueryPageCountLimit, exclusiveStartKey, filterConditionExpression, projectedAttributes...)
	}
}

func (d *DynamoDB) scanItemsWithTrace(resultItemsPtr interface{},
	timeOutDuration *time.Duration,
	consistentRead *bool,
	indexName *string,
	pageLimit *int64,
	pagedQuery bool,
	pagedQueryPageCountLimit *int64,
	exclusiveStartKey map[string]*dynamodb.AttributeValue,
	filterConditionExpression expression.ConditionBuilder, projectedAttributes ...string) (prevEvalKey map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {
	trace := xray.NewSegment("DynamoDB-ScanItems", d._parentSegment)
	defer trace.Close()
	defer func() {
		if ddbErr != nil {
			_ = trace.Seg.AddError(fmt.Errorf(ddbErr.ErrorMessage))
		}
	}()

	if d.cn == nil {
		ddbErr = d.handleError(errors.New("DynamoDB Connection is Required"))
		return nil, ddbErr
	}

	if util.LenTrim(d.TableName) <= 0 {
		ddbErr = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return nil, ddbErr
	}

	// result items pointer must be set
	if resultItemsPtr == nil {
		ddbErr = d.handleError(errors.New("DynamoDB ScanItems Failed: " + "ResultItems is Nil"))
		return nil, ddbErr
	}

	// execute dynamodb service
	var result *dynamodb.ScanOutput

	trace.Capture("ScanItems", func() error {
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

			projSet = true
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
			ddbErr = d.handleError(err, "DynamoDB ScanItems Failed: (Expression NewBuilder)")
			return fmt.Errorf(ddbErr.ErrorMessage)
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
			params.ExpressionAttributeNames = expr.Names()
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

		if indexName != nil && util.LenTrim(*indexName) > 0 {
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

		subTrace := trace.NewSubSegment("ScanItems_Do")
		defer subTrace.Close()

		// create timeout context
		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			result, err = d.do_Scan(params, pagedQuery, pagedQueryPageCountLimit, ctx)
		} else {
			result, err = d.do_Scan(params, pagedQuery, pagedQueryPageCountLimit, subTrace.Ctx)
		}

		if err != nil {
			ddbErr = d.handleError(err, "DynamoDB ScanItems Failed: (ScanItems)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		}

		if result == nil {
			return nil
		}

		// unmarshal result items to target object map
		if err = dynamodbattribute.UnmarshalListOfMaps(result.Items, resultItemsPtr); err != nil {
			ddbErr = d.handleError(err, "DynamoDB ScanItems Failed: (Unmarshal Result Items)")
			return fmt.Errorf(ddbErr.ErrorMessage)
		} else {
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":                 d.TableName,
			"IndexName":                 aws.StringValue(indexName),
			"ExclusiveStartKey":         exclusiveStartKey,
			"FilterConditionExpression": filterConditionExpression,
		},
	})

	// scan items successful
	if result != nil {
		return result.LastEvaluatedKey, ddbErr
	} else {
		return nil, ddbErr
	}
}

func (d *DynamoDB) scanItemsNormal(resultItemsPtr interface{},
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

	// execute dynamodb service
	var result *dynamodb.ScanOutput

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

		projSet = true
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
		params.ExpressionAttributeNames = expr.Names()
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

	if indexName != nil && util.LenTrim(*indexName) > 0 {
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

	if result == nil {
		return nil, d.handleError(err, "DynamoDB ScanItems Failed: (ScanItems)")
	}

	// unmarshal result items to target object map
	if err = dynamodbattribute.UnmarshalListOfMaps(result.Items, resultItemsPtr); err != nil {
		ddbErr = d.handleError(err, "DynamoDB ScanItems Failed: (Unmarshal Result Items)")
	} else {
		ddbErr = nil
	}

	// scan items successful
	return result.LastEvaluatedKey, ddbErr
}

// ScanItemsWithRetry handles dynamodb retries in case action temporarily fails
//
// warning
//
//	projectedAttributes = if specified, must include PartitionKey (Hash key) typically "PK" as the first attribute in projected attributes
func (d *DynamoDB) ScanItemsWithRetry(maxRetries uint,
	resultItemsPtr interface{},
	timeOutDuration *time.Duration,
	consistentRead *bool,
	indexName *string,
	pageLimit *int64,
	pagedQuery bool,
	pagedQueryPageCountLimit *int64,
	exclusiveStartKey map[string]*dynamodb.AttributeValue,
	filterConditionExpression expression.ConditionBuilder, projectedAttributes ...string) (prevEvalKey map[string]*dynamodb.AttributeValue, ddbErr *DynamoDBError) {
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 10 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	} else if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}

	if prevEvalKey, ddbErr = d.ScanItems(resultItemsPtr, util.DurationPtr(timeout), consistentRead, indexName, pageLimit,
		pagedQuery, pagedQueryPageCountLimit,
		exclusiveStartKey, filterConditionExpression, projectedAttributes...); ddbErr != nil {
		// has error
		if maxRetries > 0 {
			if ddbErr.AllowRetry {
				if ddbErr.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("ScanItemsWithRetry Failed: " + ddbErr.ErrorMessage)
				return d.ScanItemsWithRetry(maxRetries-1,
					resultItemsPtr, util.DurationPtr(timeout), consistentRead, indexName, pageLimit,
					pagedQuery, pagedQueryPageCountLimit,
					exclusiveStartKey, filterConditionExpression, projectedAttributes...)
			} else {
				if ddbErr.SuppressError {
					log.Println("ScanItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return nil, nil
				} else {
					return nil, &DynamoDBError{
						ErrorMessage:      "ScanItemsWithRetry Failed: " + ddbErr.ErrorMessage,
						SuppressError:     false,
						AllowRetry:        false,
						RetryNeedsBackOff: false,
					}
				}
			}
		} else {
			if ddbErr.SuppressError {
				log.Println("ScanItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return nil, nil
			} else {
				return nil, &DynamoDBError{
					ErrorMessage:      "ScanItemsWithRetry Failed: (MaxRetries = 0) " + ddbErr.ErrorMessage,
					SuppressError:     false,
					AllowRetry:        false,
					RetryNeedsBackOff: false,
				}
			}
		}
	} else {
		// no error
		return prevEvalKey, nil
	}
}

// =====================================================================================================================
// ScanPagedItems Functions
// =====================================================================================================================

// ScanPagedItemsWithRetry will scan dynamodb items in given table using paged mode with retry, project results, using filter expression
// >>> DO NOT USE SCAN IF POSSIBLE - SCAN IS NOT EFFICIENT ON RCU <<<
//
// parameters:
//
//	maxRetries = required, max number of auto retries per paged query
//	pagedSlicePtr = required, working variable to store paged query (actual return items list is via return variable)
//	resultSlicePtr = required, pointer to items list struct to contain queried result; i.e. []Item{} where Item is struct; if projected attributes less than struct fields, unmatched is defaulted
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//	consistentRead = (always false)
//	indexName = optional, global secondary index or local secondary index name to help in scan operation
//	pageLimit = (always 100)
//	pagedQuery = (always true)
//	pagedQueryPageCountLimit = (always 25)
//	exclusiveStartKey = (always internally controlled during paged query)
//
//	filterConditionExpression = required; ATTRIBUTES ARE CASE SENSITIVE; sets the scan filter condition;
//		Usage Syntax:
//			1) expFilter := expression.Name(xyz).Equals(expression.Value(abc))
//			2) expFilter := expression.Name(xyz).Equals(expression.Value(abc)).And(...)
//			3) Assign expFilter into filterConditionExpression
//
//	projectedAttributes = (always project all attributes)
//
// Return Values:
//
//	returnItemsList = interface of slice returned, representing the items found during scan
//	err = error if encountered
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) ScanPagedItemsWithRetry(maxRetries uint,
	pagedSlicePtr interface{},
	resultSlicePtr interface{},
	timeOutDuration *time.Duration,
	indexName string,
	filterConditionExpression expression.ConditionBuilder) (returnItemsList interface{}, err error) {

	if pagedSlicePtr == nil {
		return nil, fmt.Errorf("PagedSlicePtr Identifies Working Slice Pointer During Scan and is Required")
	} else {
		if valPaged := reflect.ValueOf(pagedSlicePtr); valPaged.Kind() != reflect.Ptr {
			return nil, fmt.Errorf("PagedSlicePtr Expected To Be Slice Pointer (Not Ptr)")
		} else if valPaged.Elem().Kind() != reflect.Slice {
			return nil, fmt.Errorf("PagedSlicePtr Expected To Be Slice Pointer (Not Slice)")
		}
	}

	if resultSlicePtr == nil {
		return nil, fmt.Errorf("ResultSlicePtr Contains Scan Result in Slice Pointer and is Required")
	} else {
		if valResult := reflect.ValueOf(resultSlicePtr); valResult.Kind() != reflect.Ptr {
			return nil, fmt.Errorf("ResultSlicePtr Expected To Be Slice Pointer (Not Ptr)")
		} else if valResult.Elem().Kind() != reflect.Slice {
			return nil, fmt.Errorf("ResultSlicePtr Expected To Be Slice Pointer (Not Slice)")
		}
	}

	var prevEvalKey map[string]*dynamodb.AttributeValue
	prevEvalKey = nil

	var e *DynamoDBError

	pageLimit := int64(100)
	pagedQueryPageCountLimit := int64(25)

	var indexNamePtr *string

	if util.LenTrim(indexName) > 0 {
		indexNamePtr = aws.String(indexName)
	} else {
		indexNamePtr = nil
	}

	for {
		if prevEvalKey, e = d.ScanItemsWithRetry(maxRetries, pagedSlicePtr, timeOutDuration, nil, indexNamePtr,
			aws.Int64(pageLimit), true, aws.Int64(pagedQueryPageCountLimit), prevEvalKey, filterConditionExpression); e != nil {
			// error
			return nil, fmt.Errorf("ScanPagedItemsWithRetry Failed: %s", e)
		} else {
			// success
			var valTarget reflect.Value

			if reflect.TypeOf(resultSlicePtr).Kind() == reflect.Ptr {
				valTarget = reflect.ValueOf(resultSlicePtr).Elem()
			} else {
				valTarget = reflect.ValueOf(resultSlicePtr)
			}

			val := reflect.AppendSlice(valTarget, reflect.ValueOf(pagedSlicePtr).Elem())
			resultSlicePtr = val.Interface()

			if prevEvalKey == nil {
				break
			}

			if len(prevEvalKey) == 0 {
				break
			}
		}
	}

	return resultSlicePtr, nil
}

// =====================================================================================================================
// BatchWriteItems Functions
// =====================================================================================================================

// BatchWriteItems will group up to 25 put and delete items in a single batch, and perform actions in parallel against dynamodb for better write efficiency，
// To update items, use UpdateItem instead for each item needing to be updated instead, BatchWriteItems does not support update items
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// parameters:
//
//	putItemsSet = slice of item struct objects to add to table (combine of putItems and deleteItems cannot exceed 25)
//		1) Each element of slice is an struct object to be added, struct object must have PK, SK or another named primary key for example, and other attributes as needed
//		2) putItems interface{} = Expects SLICE of STRUCT OBJECTS
//
//	deleteKeys = slice of search keys (as defined by DynamoDBTableKeys struct) to remove from table (combine of putItems and deleteKeys cannot exceed 25)
//		1) Each element of slice is an struct object of DynamoDBTableKeys
//
//	timeOutDuration = optional, timeout duration sent via context to scan method; nil if not using timeout duration
//
// return values:
//
//	successCount = total number of item actions succeeded
//	unprocessedItems = Slice of Table based item actions did not succeed is returned; nil means all processed
//	err = if method call failed, error is returned
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) BatchWriteItems(
	putItemsSet []*DynamoDBTransactionWritePutItemsSet,
	deleteKeys []*DynamoDBTableKeys,
	timeOutDuration *time.Duration) (successCount int, unprocessedItems []*DynamoDBUnprocessedItemsAndKeys, err *DynamoDBError) {

	if xray.XRayServiceOn() {
		return d.batchWriteItemsWithTrace(putItemsSet, deleteKeys, timeOutDuration)
	} else {
		return d.batchWriteItemsNormal(putItemsSet, deleteKeys, timeOutDuration)
	}
}

func (d *DynamoDB) batchWriteItemsWithTrace(putItemsSet []*DynamoDBTransactionWritePutItemsSet,
	deleteKeys []*DynamoDBTableKeys,
	timeOutDuration *time.Duration) (successCount int, unprocessedItems []*DynamoDBUnprocessedItemsAndKeys, err *DynamoDBError) {

	trace := xray.NewSegment("DynamoDB-BatchWriteItems", d._parentSegment)
	defer trace.Close()
	defer func() {
		if err != nil {
			_ = trace.Seg.AddError(fmt.Errorf(err.ErrorMessage))
		}
	}()

	if d.cn == nil {
		err = d.handleError(errors.New("DynamoDB Connection is Required"))
		return 0, nil, err
	}

	if util.LenTrim(d.TableName) <= 0 {
		err = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return 0, nil, err
	}

	// validate input parameters
	if putItemsSet == nil && deleteKeys == nil {
		err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Both Cannot Be Nil"))
		return 0, nil, err
	}

	if len(putItemsSet) > 0 && len(deleteKeys) > 0 {
		err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Cannot Be Used Together At the Same Time"))
		return 0, nil, err
	}

	trace.Capture("BatchWriteItems", func() error {
		// marshal put and delete objects (outer map is table name)
		var putTableItemsAv map[string][]map[string]*dynamodb.AttributeValue
		var deleteTableKeysAv map[string][]map[string]*dynamodb.AttributeValue

		if putItemsSet != nil && len(putItemsSet) > 0 {
			for _, putSet := range putItemsSet {
				if putSet != nil && putSet.PutItems != nil {
					if md, e := putSet.MarshalPutItems(); e != nil {
						successCount = 0
						unprocessedItems = nil
						err = d.handleError(e, "DynamoDB BatchWriteItems Failed: (PutItems MarshalMap)")
						return fmt.Errorf(err.ErrorMessage)
					} else {
						tableName := d.TableName

						if util.LenTrim(putSet.TableNameOverride) > 0 {
							tableName = putSet.TableNameOverride
						}

						if putTableItemsAv == nil {
							putTableItemsAv = make(map[string][]map[string]*dynamodb.AttributeValue)
						}

						if putTableItemsAv[tableName] == nil {
							putTableItemsAv[tableName] = make([]map[string]*dynamodb.AttributeValue, 0)
						}

						for _, v := range md {
							putTableItemsAv[tableName] = append(putTableItemsAv[tableName], v)
						}
					}
				}
			}
		}

		if deleteKeys != nil {
			if len(deleteKeys) > 0 {
				for _, v := range deleteKeys {
					if v != nil {
						if m, e := dynamodbattribute.MarshalMap(v); e != nil {
							successCount = 0
							unprocessedItems = nil
							err = d.handleError(e, "DynamoDB BatchWriteItems Failed: (DeleteKeys MarshalMap)")
							return fmt.Errorf(err.ErrorMessage)
						} else {
							if m != nil {
								tableName := d.TableName

								if util.LenTrim(tableName) > 0 {
									tableName = v.TableNameOverride
								}

								if deleteTableKeysAv == nil {
									deleteTableKeysAv = make(map[string][]map[string]*dynamodb.AttributeValue)
								}

								if deleteTableKeysAv[tableName] == nil {
									deleteTableKeysAv[tableName] = make([]map[string]*dynamodb.AttributeValue, 0)
								}

								deleteTableKeysAv[tableName] = append(deleteTableKeysAv[tableName], m)
							} else {
								successCount = 0
								unprocessedItems = nil
								err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: (DeleteKeys MarshalMap) " + "DeleteKey Marshal Result Object Nil"))
								return fmt.Errorf(err.ErrorMessage)
							}
						}
					}
				}
			}
		}

		putCount := 0
		deleteCount := 0

		if putTableItemsAv != nil {
			// loop thru map to get count
			for _, v := range putTableItemsAv {
				if v != nil {
					putCount += len(v)
				}
			}
		}

		if deleteTableKeysAv != nil {
			// loop thru map to get count
			for _, v := range deleteTableKeysAv {
				if v != nil {
					deleteCount += len(v)
				}
			}
		}

		if (putCount+deleteCount) <= 0 || (putCount+deleteCount) > 25 {
			successCount = 0
			unprocessedItems = nil
			err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Count Must Be 1 to 25 Only"))
			return fmt.Errorf(err.ErrorMessage)
		}

		// holder of delete and put item write requests
		requestItems := make(map[string][]*dynamodb.WriteRequest)

		// define requestItems wrapper
		if deleteCount > 0 {
			for tblName, attr := range deleteTableKeysAv {
				if util.LenTrim(tblName) > 0 && len(attr) > 0 {
					for _, v := range attr {
						requestItems[tblName] = append(requestItems[tblName], &dynamodb.WriteRequest{
							DeleteRequest: &dynamodb.DeleteRequest{
								Key: v,
							},
						})
					}
				}
			}
		}

		if putCount > 0 {
			for tblName, attr := range putTableItemsAv {
				if util.LenTrim(tblName) > 0 && len(attr) > 0 {
					for _, v := range attr {
						requestItems[tblName] = append(requestItems[tblName], &dynamodb.WriteRequest{
							PutRequest: &dynamodb.PutRequest{
								Item: v,
							},
						})
					}
				}
			}
		}

		// compose batch write params
		params := &dynamodb.BatchWriteItemInput{
			RequestItems: requestItems,
		}

		// record params payload
		d.LastExecuteParamsPayload = "BatchWriteItems = " + params.String()

		// execute batch write action
		var result *dynamodb.BatchWriteItemOutput
		var err1 error

		subTrace := trace.NewSubSegment("BatchWriteItems_Do")
		defer subTrace.Close()

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			result, err1 = d.do_BatchWriteItem(params, ctx)
		} else {
			result, err1 = d.do_BatchWriteItem(params, subTrace.Ctx)
		}

		if err1 != nil {
			successCount = 0
			unprocessedItems = nil
			err = d.handleError(err1, "DynamoDB BatchWriteItems Failed: (BatchWriteItem)")
			return fmt.Errorf(err.ErrorMessage)
		}

		if result == nil {
			successCount = 0
			unprocessedItems = nil
			err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: (BatchWriteItem) " + "Result Nil"))
			return fmt.Errorf(err.ErrorMessage)
		}

		// evaluate unprocessed items
		if result.UnprocessedItems != nil && len(result.UnprocessedItems) > 0 {
			unprocessedCount := 0

			for tblName, unprocessed := range result.UnprocessedItems {
				if util.LenTrim(tblName) > 0 && unprocessed != nil && len(unprocessed) > 0 {
					unprocessedList := &DynamoDBUnprocessedItemsAndKeys{
						TableName: tblName,
					}

					for _, v := range unprocessed {
						if v != nil {
							if v.PutRequest != nil && v.PutRequest.Item != nil {
								unprocessedList.PutItems = append(unprocessedList.PutItems, v.PutRequest.Item)
								unprocessedCount++
							}

							if v.DeleteRequest != nil && v.DeleteRequest.Key != nil {
								var o DynamoDBTableKeys

								if e := dynamodbattribute.UnmarshalMap(v.DeleteRequest.Key, &o); e == nil {
									unprocessedList.DeleteKeys = append(unprocessedList.DeleteKeys, &o)
								}

								unprocessedCount++
							}
						}
					}

					unprocessedItems = append(unprocessedItems, unprocessedList)
				}
			}

			successCount = deleteCount + putCount - unprocessedCount
			err = nil
			return nil
		}

		successCount = deleteCount + putCount
		unprocessedItems = nil
		err = nil
		return nil
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":  d.TableName,
			"PutItems":   putItemsSet,
			"DeleteKeys": deleteKeys,
		},
	})

	// batch put and delete items successful
	return successCount, unprocessedItems, err
}

func (d *DynamoDB) batchWriteItemsNormal(putItemsSet []*DynamoDBTransactionWritePutItemsSet,
	deleteKeys []*DynamoDBTableKeys,
	timeOutDuration *time.Duration) (successCount int, unprocessedItems []*DynamoDBUnprocessedItemsAndKeys, err *DynamoDBError) {

	if d.cn == nil {
		return 0, nil, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return 0, nil, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	// validate input parameters
	if putItemsSet == nil && deleteKeys == nil {
		return 0, nil, d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Both Cannot Be Nil"))
	}

	if len(putItemsSet) > 0 && len(deleteKeys) > 0 {
		err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Cannot Be Used Together At the Same Time"))
		return 0, nil, err
	}

	// marshal put and delete objects
	var putTableItemsAv map[string][]map[string]*dynamodb.AttributeValue
	var deleteTableKeysAv map[string][]map[string]*dynamodb.AttributeValue

	if putItemsSet != nil && len(putItemsSet) > 0 {
		for _, putSet := range putItemsSet {
			if putSet != nil && putSet.PutItems != nil {
				if md, e := putSet.MarshalPutItems(); e != nil {
					successCount = 0
					unprocessedItems = nil
					err = d.handleError(e, "DynamoDB BatchWriteItems Failed: (PutItems MarshalMap)")
					return successCount, unprocessedItems, err
				} else {
					tableName := d.TableName

					if util.LenTrim(putSet.TableNameOverride) > 0 {
						tableName = putSet.TableNameOverride
					}

					if putTableItemsAv == nil {
						putTableItemsAv = make(map[string][]map[string]*dynamodb.AttributeValue)
					}

					if putTableItemsAv[tableName] == nil {
						putTableItemsAv[tableName] = make([]map[string]*dynamodb.AttributeValue, 0)
					}

					for _, v := range md {
						putTableItemsAv[tableName] = append(putTableItemsAv[tableName], v)
					}
				}
			}
		}
	}

	if deleteKeys != nil {
		if len(deleteKeys) > 0 {
			for _, v := range deleteKeys {
				if v != nil {
					if m, e := dynamodbattribute.MarshalMap(v); e != nil {
						successCount = 0
						unprocessedItems = nil
						err = d.handleError(e, "DynamoDB BatchWriteItems Failed: (DeleteKeys MarshalMap)")
						return successCount, unprocessedItems, err
					} else {
						if m != nil {
							tableName := d.TableName

							if util.LenTrim(tableName) > 0 {
								tableName = v.TableNameOverride
							}

							if deleteTableKeysAv == nil {
								deleteTableKeysAv = make(map[string][]map[string]*dynamodb.AttributeValue)
							}

							if deleteTableKeysAv[tableName] == nil {
								deleteTableKeysAv[tableName] = make([]map[string]*dynamodb.AttributeValue, 0)
							}

							deleteTableKeysAv[tableName] = append(deleteTableKeysAv[tableName], m)
						} else {
							successCount = 0
							unprocessedItems = nil
							err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: (DeleteKeys MarshalMap) " + "DeleteKey Marshal Result Object Nil"))
							return successCount, unprocessedItems, err
						}
					}
				}
			}
		}
	}

	putCount := 0
	deleteCount := 0

	if putTableItemsAv != nil {
		// loop thru map to get count
		for _, v := range putTableItemsAv {
			if v != nil {
				putCount += len(v)
			}
		}
	}

	if deleteTableKeysAv != nil {
		// loop thru map to get count
		for _, v := range deleteTableKeysAv {
			if v != nil {
				deleteCount += len(v)
			}
		}
	}

	if (putCount+deleteCount) <= 0 || (putCount+deleteCount) > 25 {
		successCount = 0
		unprocessedItems = nil
		err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: " + "PutItems and DeleteKeys Count Must Be 1 to 25 Only"))
		return successCount, unprocessedItems, err
	}

	// holder of delete and put item write requests
	requestItems := make(map[string][]*dynamodb.WriteRequest)

	// define requestItems wrapper
	if deleteCount > 0 {
		for tblName, attr := range deleteTableKeysAv {
			if util.LenTrim(tblName) > 0 && len(attr) > 0 {
				for _, v := range attr {
					requestItems[tblName] = append(requestItems[tblName], &dynamodb.WriteRequest{
						DeleteRequest: &dynamodb.DeleteRequest{
							Key: v,
						},
					})
				}
			}
		}
	}

	if putCount > 0 {
		for tblName, attr := range putTableItemsAv {
			if util.LenTrim(tblName) > 0 && len(attr) > 0 {
				for _, v := range attr {
					requestItems[tblName] = append(requestItems[tblName], &dynamodb.WriteRequest{
						PutRequest: &dynamodb.PutRequest{
							Item: v,
						},
					})
				}
			}
		}
	}

	// compose batch write params
	params := &dynamodb.BatchWriteItemInput{
		RequestItems: requestItems,
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
		successCount = 0
		unprocessedItems = nil
		err = d.handleError(err1, "DynamoDB BatchWriteItems Failed: (BatchWriteItem)")
		return successCount, unprocessedItems, err
	}

	if result == nil {
		successCount = 0
		unprocessedItems = nil
		err = d.handleError(errors.New("DynamoDB BatchWriteItems Failed: (BatchWriteItem) " + "Result Nil"))
		return successCount, unprocessedItems, err
	}

	// evaluate unprocessed items
	if result.UnprocessedItems != nil && len(result.UnprocessedItems) > 0 {
		unprocessedCount := 0

		for tblName, unprocessed := range result.UnprocessedItems {
			if util.LenTrim(tblName) > 0 && unprocessed != nil && len(unprocessed) > 0 {
				unprocessedList := &DynamoDBUnprocessedItemsAndKeys{
					TableName: tblName,
				}

				for _, v := range unprocessed {
					if v != nil {
						if v.PutRequest != nil && v.PutRequest.Item != nil {
							unprocessedList.PutItems = append(unprocessedList.PutItems, v.PutRequest.Item)
							unprocessedCount++
						}

						if v.DeleteRequest != nil && v.DeleteRequest.Key != nil {
							var o DynamoDBTableKeys

							if e := dynamodbattribute.UnmarshalMap(v.DeleteRequest.Key, &o); e == nil {
								unprocessedList.DeleteKeys = append(unprocessedList.DeleteKeys, &o)
							}

							unprocessedCount++
						}
					}
				}

				unprocessedItems = append(unprocessedItems, unprocessedList)
			}
		}

		successCount = deleteCount + putCount - unprocessedCount
		err = nil
		return successCount, unprocessedItems, err
	}

	successCount = deleteCount + putCount
	unprocessedItems = nil
	err = nil

	// batch put and delete items successful
	return successCount, unprocessedItems, err
}

// BatchWriteItemsWithRetry handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) BatchWriteItemsWithRetry(maxRetries uint,
	putItemsSet []*DynamoDBTransactionWritePutItemsSet, deleteKeys []*DynamoDBTableKeys,
	timeOutDuration *time.Duration) (successCount int, unprocessedItems []*DynamoDBUnprocessedItemsAndKeys, err *DynamoDBError) {

	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 10 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	} else if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}

	if successCount, unprocessedItems, err = d.BatchWriteItems(putItemsSet, deleteKeys, util.DurationPtr(timeout)); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("BatchWriteItemsWithRetry Failed: " + err.ErrorMessage)
				return d.BatchWriteItemsWithRetry(maxRetries-1, putItemsSet, deleteKeys, util.DurationPtr(timeout))
			} else {
				if err.SuppressError {
					log.Println("BatchWriteItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return 0, nil, nil
				} else {
					return 0, nil, &DynamoDBError{
						ErrorMessage:                      "BatchWriteItemsWithRetry Failed: " + err.ErrorMessage,
						SuppressError:                     false,
						AllowRetry:                        false,
						RetryNeedsBackOff:                 false,
						TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("BatchWriteItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return 0, nil, nil
			} else {
				return 0, nil, &DynamoDBError{
					ErrorMessage:                      "BatchWriteItemsWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:                     false,
					AllowRetry:                        false,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
				}
			}
		}
	} else {
		// no error
		return successCount, unprocessedItems, nil
	}
}

// =====================================================================================================================
// BatchGetItems Functions
// =====================================================================================================================

// BatchGetItems accepts one or more DynamoDBMultiGetRequestResponse objects, and retrieved result items would be stored within
// each of the corresponding ResultItemSlicePtr objects
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// warning
//
//	!!! If Attribute Projection is to be specified, make sure to include PK, otherwise nothing would yield in return !!!
//
// parameters:
//
//	 timeOutDuration = (optional) set if having timeout context for the operation
//		multiGetRequestResponse = one or more DynamoDBMultiGetRequestResponse objects, which contains the search keys and result items slice pointer
//
// return values:
//
//	notFound = true if no items found; if error encountered, this field returns false with error field filled
//	err = if error is encountered, this field will be filled; otherwise nil
//
// notes:
//
//	item struct tags
//		use `json:"" dynamodbav:""`
//			json = sets the name used in json
//			dynamodbav = sets the name used in dynamodb
//		reference child element
//			if struct has field with complex type (another struct), to reference it in code, use the parent struct field dot child field notation
//				Info in parent struct with struct tag as info; to reach child element: info.xyz
func (d *DynamoDB) BatchGetItems(timeOutDuration *time.Duration, multiGetRequestResponse ...*DynamoDBMultiGetRequestResponse) (notFound bool, err *DynamoDBError) {
	if xray.XRayServiceOn() {
		return d.batchGetItemsWithTrace(timeOutDuration, multiGetRequestResponse...)
	} else {
		return d.batchGetItemsNormal(timeOutDuration, multiGetRequestResponse...)
	}
}

func (d *DynamoDB) batchGetItemsWithTrace(timeOutDuration *time.Duration, multiGetRequestResponse ...*DynamoDBMultiGetRequestResponse) (notFound bool, err *DynamoDBError) {

	trace := xray.NewSegment("DynamoDB-BatchGetItems", d._parentSegment)

	defer trace.Close()
	defer func() {
		if err != nil {
			_ = trace.Seg.AddError(fmt.Errorf(err.ErrorMessage))
		}
	}()

	if d.cn == nil {
		err = d.handleError(errors.New("DynamoDB Connection is Required"))
		return false, err
	}

	if util.LenTrim(d.TableName) <= 0 {
		err = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return false, err
	}

	if util.LenTrim(d.PKName) <= 0 {
		err = d.handleError(errors.New("DynamoDB Partition Key Name is Required"))
		return false, err
	}

	if multiGetRequestResponse == nil {
		err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse is Nil"))
		return false, err
	}

	if len(multiGetRequestResponse) <= 0 {
		err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse is Empty"))
		return false, err
	}

	searchCount := 0
	foundTableNames := make(map[string]int)

	for i := 0; i < len(multiGetRequestResponse); i++ {
		if multiGetRequestResponse[i] == nil {
			err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] Element is Nil"))
			return false, err
		} else {
			if multiGetRequestResponse[i].SearchKeys == nil {
				err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] SearchKeys is Nil"))
				return false, err
			}

			if len(multiGetRequestResponse[i].SearchKeys) <= 0 {
				err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] SearchKeys is Empty"))
				return false, err
			}

			if multiGetRequestResponse[i].ResultItemsSlicePtr == nil {
				err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] ResultItemsSlicePtr is Nil"))
				return false, err
			}

			if reflect.TypeOf(multiGetRequestResponse[i].ResultItemsSlicePtr).Kind() != reflect.Ptr {
				err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
				return false, err
			}

			if reflect.ValueOf(multiGetRequestResponse[i].ResultItemsSlicePtr).Elem().Kind() != reflect.Slice {
				err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
				return false, err
			}

			searchCount += len(multiGetRequestResponse[i].SearchKeys)

			if util.LenTrim(multiGetRequestResponse[i].TableName) == 0 {
				multiGetRequestResponse[i].TableName = d.TableName
				multiGetRequestResponse[i].PKName = d.PKName
				multiGetRequestResponse[i].SKName = d.SKName
			} else {
				if strings.ToUpper(multiGetRequestResponse[i].TableName) == strings.ToUpper(d.TableName) {
					if util.LenTrim(multiGetRequestResponse[i].PKName) == 0 {
						multiGetRequestResponse[i].PKName = d.PKName
					}

					if util.LenTrim(multiGetRequestResponse[i].SKName) == 0 {
						multiGetRequestResponse[i].SKName = d.SKName
					}
				} else {
					if util.LenTrim(multiGetRequestResponse[i].PKName) == 0 {
						multiGetRequestResponse[i].PKName = "PK" // default
					}

					if util.LenTrim(multiGetRequestResponse[i].SKName) == 0 {
						multiGetRequestResponse[i].SKName = "SK" // default, might not be used, actual code decides at a later point
					}
				}
			}

			if _, ok := foundTableNames[multiGetRequestResponse[i].TableName]; ok {
				foundTableNames[multiGetRequestResponse[i].TableName]++

				if foundTableNames[multiGetRequestResponse[i].TableName] > 1 {
					err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] Table Name Cannot Duplicate In MultiGetRequestResponse Slice"))
					return false, err
				}
			} else {
				foundTableNames[multiGetRequestResponse[i].TableName] = 1
			}
		}
	}

	if searchCount > 100 {
		err = d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "SearchKeys Maximum is 100"))
		return false, err
	}

	trace.Capture("BatchGetItems", func() error {
		//
		// prepare batch get items request
		//
		requestItems := make(map[string]*dynamodb.KeysAndAttributes)

		for _, searchSet := range multiGetRequestResponse {
			//
			// marshal attributes key map from search set
			//
			if keysAv, keysErr := searchSet.MarshalSearchKeyValueMaps(); keysErr != nil {
				notFound = false
				err = d.handleError(keysErr, "DynamoDB BatchGetItems Failed: (SearchKey Marshal)")
				return fmt.Errorf(err.ErrorMessage)
			} else {
				// assign keys to request items
				requestItems[searchSet.TableName] = &dynamodb.KeysAndAttributes{
					Keys: keysAv,
				}

				// set projection
				if searchSet.ProjectedAttributes != nil {
					if projExpr, projAttr, projErr := searchSet.ProjectedAttributes.BuildProjectionParameters(); projErr != nil {
						notFound = false
						err = d.handleError(projErr, "DynamoDB BatchGetItems Failed: (Projecting Attributes)")
						return fmt.Errorf(err.ErrorMessage)
					} else {
						// assign projection expression
						if projExpr != nil && (projAttr != nil && len(projAttr) > 0) {
							requestItems[searchSet.TableName].ProjectionExpression = projExpr
							requestItems[searchSet.TableName].ExpressionAttributeNames = projAttr
						}
					}
				}

				// set consistent read
				if searchSet.ConsistentRead != nil {
					requestItems[searchSet.TableName].ConsistentRead = searchSet.ConsistentRead
				}
			}
		}

		// define params
		params := &dynamodb.BatchGetItemInput{
			RequestItems: requestItems,
		}

		// record params payload
		d.LastExecuteParamsPayload = "BatchGetItems = " + params.String()

		// execute batch
		var result *dynamodb.BatchGetItemOutput

		subTrace := trace.NewSubSegment("BatchGetItems_Do")
		defer subTrace.Close()

		var err1 error

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			result, err1 = d.do_BatchGetItem(params, ctx)
		} else {
			result, err1 = d.do_BatchGetItem(params, subTrace.Ctx)
		}

		// evaluate batch execute result
		if err1 != nil {
			notFound = false
			err = d.handleError(err1, "DynamoDB BatchGetItems Failed: (BatchGetItem)")
			return fmt.Errorf(err.ErrorMessage)
		}

		if result.Responses == nil {
			// not found - nil
			notFound = true
			err = nil
			return nil
		} else if len(result.Responses) <= 0 {
			// not found - empty
			notFound = true
			err = nil
			return nil
		} else {
			//
			// loop thru each searchKey set's TableName to receive response items into its corresponding ResultItemsSlicePtr
			//
			totalCount := 0

			for _, searchSet := range multiGetRequestResponse {
				if resp := result.Responses[searchSet.TableName]; resp != nil && len(resp) > 0 {
					// unmarshal results
					if err1 = dynamodbattribute.UnmarshalListOfMaps(resp, searchSet.ResultItemsSlicePtr); err1 != nil {
						notFound = false
						err = d.handleError(err1, "DynamoDB BatchGetItems Failed: (Unmarshal ResultItems)")
						return fmt.Errorf(err.ErrorMessage)
					} else {
						// unmarshal successful
						searchSet.ResultItemsCount = len(resp)
						totalCount += searchSet.ResultItemsCount
					}
				} else {
					searchSet.ResultItemsCount = 0
				}
			}

			// completed
			notFound = totalCount <= 0
			err = nil
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName":   d.TableName,
			"ReqRespList": multiGetRequestResponse,
		},
	})

	return notFound, err
}

func (d *DynamoDB) batchGetItemsNormal(timeOutDuration *time.Duration, multiGetRequestResponse ...*DynamoDBMultiGetRequestResponse) (notFound bool, err *DynamoDBError) {

	if d.cn == nil {
		return false, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return false, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	if util.LenTrim(d.PKName) <= 0 {
		return false, d.handleError(errors.New("DynamoDB Partition Key Name is Required"))
	}

	if multiGetRequestResponse == nil {
		return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse is Nil"))
	}

	if len(multiGetRequestResponse) <= 0 {
		return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse is Empty"))
	}

	searchCount := 0
	foundTableNames := make(map[string]int)

	for i := 0; i < len(multiGetRequestResponse); i++ {
		if multiGetRequestResponse[i] == nil {
			return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] Element is Nil"))
		} else {
			if multiGetRequestResponse[i].SearchKeys == nil {
				return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] SearchKeys is Nil"))
			}

			if len(multiGetRequestResponse[i].SearchKeys) <= 0 {
				return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] SearchKeys is Empty"))
			}

			if multiGetRequestResponse[i].ResultItemsSlicePtr == nil {
				return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] ResultItemsSlicePtr is Nil"))
			}

			if reflect.TypeOf(multiGetRequestResponse[i].ResultItemsSlicePtr).Kind() != reflect.Ptr {
				return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
			}

			if reflect.ValueOf(multiGetRequestResponse[i].ResultItemsSlicePtr).Elem().Kind() != reflect.Slice {
				return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
			}

			searchCount += len(multiGetRequestResponse[i].SearchKeys)

			if util.LenTrim(multiGetRequestResponse[i].TableName) == 0 {
				multiGetRequestResponse[i].TableName = d.TableName
				multiGetRequestResponse[i].PKName = d.PKName
				multiGetRequestResponse[i].SKName = d.SKName
			} else {
				if strings.ToUpper(multiGetRequestResponse[i].TableName) == strings.ToUpper(d.TableName) {
					if util.LenTrim(multiGetRequestResponse[i].PKName) == 0 {
						multiGetRequestResponse[i].PKName = d.PKName
					}

					if util.LenTrim(multiGetRequestResponse[i].SKName) == 0 {
						multiGetRequestResponse[i].SKName = d.SKName
					}
				} else {
					if util.LenTrim(multiGetRequestResponse[i].PKName) == 0 {
						multiGetRequestResponse[i].PKName = "PK" // default
					}

					if util.LenTrim(multiGetRequestResponse[i].SKName) == 0 {
						multiGetRequestResponse[i].SKName = "SK" // default, might not be used, actual code decides at a later point
					}
				}
			}

			if _, ok := foundTableNames[multiGetRequestResponse[i].TableName]; ok {
				foundTableNames[multiGetRequestResponse[i].TableName]++

				if foundTableNames[multiGetRequestResponse[i].TableName] > 1 {
					return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "MultiGetRequestResponse[" + util.Itoa(i) + "] TableName Cannot Duplicate In MultiGetRequestResponse Slice"))
				}
			} else {
				foundTableNames[multiGetRequestResponse[i].TableName] = 1
			}
		}
	}

	if searchCount > 100 {
		return false, d.handleError(errors.New("DynamoDB BatchGetItems Failed: " + "SearchKeys Maximum is 100"))
	}

	//
	// prepare batch get items request
	//
	requestItems := make(map[string]*dynamodb.KeysAndAttributes)

	for _, searchSet := range multiGetRequestResponse {
		//
		// marshal attributes key map from search set
		//
		if keysAv, keysErr := searchSet.MarshalSearchKeyValueMaps(); keysErr != nil {
			return false, d.handleError(keysErr, "DynamoDB BatchGetItems Failed: (SearchKey Marshal)")
		} else {
			// assign keys to request items
			requestItems[searchSet.TableName] = &dynamodb.KeysAndAttributes{
				Keys: keysAv,
			}

			// set projection
			if searchSet.ProjectedAttributes != nil {
				if projExpr, projAttr, projErr := searchSet.ProjectedAttributes.BuildProjectionParameters(); projErr != nil {
					return false, d.handleError(projErr, "DynamoDB BatchGetItems Failed: (Projecting Attributes)")
				} else {
					// assign projection expression
					if projExpr != nil && (projAttr != nil && len(projAttr) > 0) {
						requestItems[searchSet.TableName].ProjectionExpression = projExpr
						requestItems[searchSet.TableName].ExpressionAttributeNames = projAttr
					}
				}
			}

			// set consistent read
			if searchSet.ConsistentRead != nil {
				requestItems[searchSet.TableName].ConsistentRead = searchSet.ConsistentRead
			}
		}
	}

	// define params
	params := &dynamodb.BatchGetItemInput{
		RequestItems: requestItems,
	}

	// record params payload
	d.LastExecuteParamsPayload = "BatchGetItems = " + params.String()

	// execute batch
	var result *dynamodb.BatchGetItemOutput
	var err1 error

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
		// not found - nil
		return true, nil
	} else if len(result.Responses) <= 0 {
		// not found - empty
		return true, nil
	} else {
		//
		// loop thru each searchKey set's TableName to receive response items into its corresponding ResultItemsSlicePtr
		//
		totalCount := 0

		for _, searchSet := range multiGetRequestResponse {
			if resp := result.Responses[searchSet.TableName]; resp != nil && len(resp) > 0 {
				// unmarshal results
				if err1 = dynamodbattribute.UnmarshalListOfMaps(resp, searchSet.ResultItemsSlicePtr); err1 != nil {
					return false, d.handleError(err1, "DynamoDB BatchGetItems Failed: (Unmarshal ResultItems)")
				} else {
					// unmarshal successful
					searchSet.ResultItemsCount = len(resp)
					totalCount += searchSet.ResultItemsCount
				}
			} else {
				searchSet.ResultItemsCount = 0
			}
		}

		// completed
		return totalCount <= 0, nil
	}
}

// BatchGetItemsWithRetry handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) BatchGetItemsWithRetry(maxRetries uint, timeOutDuration *time.Duration, multiGetRequestResponse ...*DynamoDBMultiGetRequestResponse) (notFound bool, err *DynamoDBError) {
	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	if notFound, err = d.BatchGetItems(util.DurationPtr(timeout), multiGetRequestResponse...); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("BatchGetItemsWithRetry Failed: " + err.ErrorMessage)
				return d.BatchGetItemsWithRetry(maxRetries-1, util.DurationPtr(timeout), multiGetRequestResponse...)
			} else {
				if err.SuppressError {
					log.Println("BatchGetItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return true, nil
				} else {
					return true, &DynamoDBError{
						ErrorMessage:                      "BatchGetItemsWithRetry Failed: " + err.ErrorMessage,
						SuppressError:                     false,
						AllowRetry:                        false,
						RetryNeedsBackOff:                 false,
						TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("BatchGetItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return true, nil
			} else {
				return true, &DynamoDBError{
					ErrorMessage:                      "BatchGetItemsWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:                     false,
					AllowRetry:                        false,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
				}
			}
		}
	} else {
		// no error
		return notFound, nil
	}
}

// =====================================================================================================================
// BatchDeleteItems Functions
// =====================================================================================================================

// BatchDeleteItemsWithRetry will attempt to delete one or more records on the current table,
// will auto retry delete if temporarily failed,
// if there are deleteFailKeys, its returned, if all succeeded, nil is returned
func (d *DynamoDB) BatchDeleteItemsWithRetry(maxRetries uint,
	timeOutDuration *time.Duration,
	deleteKeys ...*DynamoDBTableKeyValue) (deleteFailKeys []*DynamoDBTableKeyValue, err error) {

	if len(deleteKeys) == 0 {
		return []*DynamoDBTableKeyValue{}, fmt.Errorf("BatchDeleteItemsWithRetry Failed: %s", err)
	}

	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	for _, key := range deleteKeys {
		if key != nil && util.LenTrim(key.PK) > 0 {
			retries := maxRetries

			if e := d.DeleteItemWithRetry(retries, key.PK, key.SK, util.DurationPtr(timeout)); e != nil {
				key.ResultError = e
				deleteFailKeys = append(deleteFailKeys, key)
			}
		}
	}

	if len(deleteFailKeys) == len(deleteKeys) {
		// all failed
		return deleteFailKeys, fmt.Errorf("BatchDeleteItemsWithRetry Failed: All Delete Actions Failed")

	} else if len(deleteFailKeys) == 0 {
		// all success
		return []*DynamoDBTableKeyValue{}, nil

	} else {
		// some failed
		return deleteFailKeys, fmt.Errorf("BatchDeleteItemsWithRetry Partial Failure: Some Delete Actions Failed")
	}
}

// =====================================================================================================================
// TransactionWriteItems Functions
// =====================================================================================================================

// TransactionWriteItems performs a transaction write action for one or more DynamoDBTransactionWrites struct objects,
// Either all success or all fail,
// Total Items Count in a Single Transaction for All transItems combined (inner elements) cannot exceed 25
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
func (d *DynamoDB) TransactionWriteItems(timeOutDuration *time.Duration, tranItems ...*DynamoDBTransactionWrites) (success bool, err *DynamoDBError) {
	if xray.XRayServiceOn() {
		return d.transactionWriteItemsWithTrace(timeOutDuration, tranItems...)
	} else {
		return d.transactionWriteItemsNormal(timeOutDuration, tranItems...)
	}
}

func (d *DynamoDB) transactionWriteItemsWithTrace(timeOutDuration *time.Duration, tranItems ...*DynamoDBTransactionWrites) (success bool, err *DynamoDBError) {

	trace := xray.NewSegment("DynamoDB-TransactionWriteItems", d._parentSegment)

	defer trace.Close()

	defer func() {
		if err != nil {
			_ = trace.Seg.AddError(fmt.Errorf(err.ErrorMessage))
		}
	}()

	if d.cn == nil {
		err = d.handleError(errors.New("DynamoDB Connection is Required"))
		return false, err
	}

	if util.LenTrim(d.TableName) <= 0 {
		err = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return false, err
	}

	if util.LenTrim(d.PKName) <= 0 {
		err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: " + "PK Name is Required"))
		return false, err
	}

	if len(tranItems) == 0 {
		err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: " + "Minimum of 1 TranItems is Required"))
		return false, err
	}

	trace.Capture("TransactionWriteItems", func() error {
		// create working data
		var items []*dynamodb.TransactWriteItem

		// loop through all tranItems slice to pre-populate transaction write items slice
		skOK := false

		for _, t := range tranItems {
			if t.DeleteItems != nil && len(t.DeleteItems) > 0 {
				for _, v := range t.DeleteItems {
					if v != nil {
						tableName := d.TableName
						pkName := d.PKName
						skName := d.SKName

						if util.LenTrim(v.TableNameOverride) > 0 {
							tableName = v.TableNameOverride
						}

						if util.LenTrim(v.PKNameOverride) > 0 {
							pkName = v.PKNameOverride
							skName = v.SKNameOverride
						}

						m := new(dynamodb.TransactWriteItem)

						md := make(map[string]*dynamodb.AttributeValue)
						md[pkName] = &dynamodb.AttributeValue{S: aws.String(v.PK)}

						if util.LenTrim(v.SK) > 0 {
							if !skOK {
								if util.LenTrim(skName) <= 0 {
									success = false
									err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "SK Name is Required"))
									return err
								} else {
									skOK = true
								}
							}

							md[skName] = &dynamodb.AttributeValue{S: aws.String(v.SK)}
						}

						m.Delete = &dynamodb.Delete{
							TableName: aws.String(tableName),
							Key:       md,
						}

						items = append(items, m)
					}
				}
			}

			if t.PutItemsSet != nil && len(t.PutItemsSet) > 0 {
				for _, putSet := range t.PutItemsSet {
					if putSet != nil && putSet.PutItems != nil {
						if md, e := putSet.MarshalPutItems(); e != nil {
							success = false
							err = d.handleError(e, "DynamoDB TransactionWriteItems Failed: (Marshal PutItems)")
							return err
						} else {
							tableName := d.TableName

							if util.LenTrim(putSet.TableNameOverride) > 0 {
								tableName = putSet.TableNameOverride
							}

							for _, v := range md {
								m := new(dynamodb.TransactWriteItem)

								m.Put = &dynamodb.Put{
									TableName: aws.String(tableName),
									Item:      v,
								}

								if util.LenTrim(putSet.ConditionExpression) > 0 {
									m.Put.ConditionExpression = aws.String(putSet.ConditionExpression)

									// set condition expression attribute values if any
									if putSet.ExpressionAttributeValues != nil && len(putSet.ExpressionAttributeValues) > 0 {
										m.Put.ExpressionAttributeValues = putSet.ExpressionAttributeValues
									}
								}

								items = append(items, m)
							}
						}
					}
				}
			}

			if t.UpdateItems != nil && len(t.UpdateItems) > 0 {
				for _, v := range t.UpdateItems {
					if v != nil {
						tableName := d.TableName

						if util.LenTrim(v.TableNameOverride) > 0 {
							tableName = v.TableNameOverride
						}

						pkName := d.PKName
						skName := d.SKName

						if util.LenTrim(v.PKNameOverride) > 0 {
							pkName = v.PKNameOverride
							skName = v.SKNameOverride
						}

						m := new(dynamodb.TransactWriteItem)

						mk := make(map[string]*dynamodb.AttributeValue)
						mk[pkName] = &dynamodb.AttributeValue{S: aws.String(v.PK)}

						if util.LenTrim(v.SK) > 0 {
							if !skOK {
								if util.LenTrim(skName) <= 0 {
									success = false
									err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "SK Name is Required"))
									return err
								} else {
									skOK = true
								}
							}

							mk[skName] = &dynamodb.AttributeValue{S: aws.String(v.SK)}
						}

						m.Update = &dynamodb.Update{
							TableName: aws.String(tableName),
							Key:       mk,
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
		}

		// items must not exceed 25
		if len(items) > 25 {
			success = false
			err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "Transaction Items May Not Exceed 25"))
			return err
		}

		if len(items) <= 0 {
			success = false
			err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "Transaction Items Minimum of 1 is Required"))
			return err
		}

		// compose transaction write items input var
		params := &dynamodb.TransactWriteItemsInput{
			TransactItems: items,
		}

		// record params payload
		d.LastExecuteParamsPayload = "TransactionWriteItems = " + params.String()

		// execute transaction write operation
		var err1 error

		subTrace := trace.NewSubSegment("TransactionWriteItems_Do")
		defer subTrace.Close()

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			_, err1 = d.do_TransactWriteItems(params, ctx)
		} else {
			_, err1 = d.do_TransactWriteItems(params, subTrace.Ctx)
		}

		if err1 != nil {
			success = false
			err = d.handleError(err1, "DynamoDB TransactionWriteItems Failed: (Transaction Canceled)")
			return err
		} else {
			success = true
			err = nil
			return nil
		}
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName": d.TableName,
			"Items":     tranItems,
		},
	})

	// success
	return success, err
}

func (d *DynamoDB) transactionWriteItemsNormal(timeOutDuration *time.Duration, tranItems ...*DynamoDBTransactionWrites) (success bool, err *DynamoDBError) {
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
		if t.DeleteItems != nil && len(t.DeleteItems) > 0 {
			for _, v := range t.DeleteItems {
				if v != nil {
					tableName := d.TableName

					if util.LenTrim(v.TableNameOverride) > 0 {
						tableName = v.TableNameOverride
					}

					pkName := d.PKName
					skName := d.SKName

					if util.LenTrim(v.PKNameOverride) > 0 {
						pkName = v.PKNameOverride
						skName = v.SKNameOverride
					}

					m := new(dynamodb.TransactWriteItem)

					md := make(map[string]*dynamodb.AttributeValue)
					md[pkName] = &dynamodb.AttributeValue{S: aws.String(v.PK)}

					if util.LenTrim(v.SK) > 0 {
						if !skOK {
							if util.LenTrim(skName) <= 0 {
								success = false
								err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "SK Name is Required"))
								return success, err
							} else {
								skOK = true
							}
						}

						md[skName] = &dynamodb.AttributeValue{S: aws.String(v.SK)}
					}

					m.Delete = &dynamodb.Delete{
						TableName: aws.String(tableName),
						Key:       md,
					}

					items = append(items, m)
				}
			}
		}

		if t.PutItemsSet != nil && len(t.PutItemsSet) > 0 {
			for _, putSet := range t.PutItemsSet {
				if putSet != nil && putSet.PutItems != nil {
					if md, e := putSet.MarshalPutItems(); e != nil {
						success = false
						err = d.handleError(e, "DynamoDB TransactionWriteItems Failed: (Marshal PutItems)")
						return success, err
					} else {
						tableName := d.TableName

						if util.LenTrim(putSet.TableNameOverride) > 0 {
							tableName = putSet.TableNameOverride
						}

						for _, v := range md {
							m := new(dynamodb.TransactWriteItem)

							m.Put = &dynamodb.Put{
								TableName: aws.String(tableName),
								Item:      v,
							}

							if util.LenTrim(putSet.ConditionExpression) > 0 {
								m.Put.ConditionExpression = aws.String(putSet.ConditionExpression)

								if putSet.ExpressionAttributeValues != nil && len(putSet.ExpressionAttributeValues) > 0 {
									m.Put.ExpressionAttributeValues = putSet.ExpressionAttributeValues
								}
							}

							items = append(items, m)
						}
					}
				}
			}
		}

		if t.UpdateItems != nil && len(t.UpdateItems) > 0 {
			for _, v := range t.UpdateItems {
				if v != nil {
					tableName := d.TableName

					if util.LenTrim(v.TableNameOverride) > 0 {
						tableName = v.TableNameOverride
					}

					pkName := d.PKName
					skName := d.SKName

					if util.LenTrim(v.PKNameOverride) > 0 {
						pkName = v.PKNameOverride
						skName = v.SKNameOverride
					}

					m := new(dynamodb.TransactWriteItem)

					mk := make(map[string]*dynamodb.AttributeValue)
					mk[pkName] = &dynamodb.AttributeValue{S: aws.String(v.PK)}

					if util.LenTrim(v.SK) > 0 {
						if !skOK {
							if util.LenTrim(skName) <= 0 {
								success = false
								err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "SK Name is Required"))
								return success, err
							} else {
								skOK = true
							}
						}

						mk[skName] = &dynamodb.AttributeValue{S: aws.String(v.SK)}
					}

					m.Update = &dynamodb.Update{
						TableName: aws.String(tableName),
						Key:       mk,
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
	}

	// items must not exceed 25
	if len(items) > 25 {
		success = false
		err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "Transaction Items May Not Exceed 25"))
		return success, err
	}

	if len(items) <= 0 {
		success = false
		err = d.handleError(errors.New("DynamoDB TransactionWriteItems Failed: (Payload Validate) " + "Transaction Items Minimum of 1 is Required"))
		return success, err
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
		success = false
		err = d.handleError(err1, "DynamoDB TransactionWriteItems Failed: (Transaction Canceled)")
		return success, err
	} else {
		return true, nil
	}
}

// TransactionWriteItemsWithRetry handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) TransactionWriteItemsWithRetry(maxRetries uint,
	timeOutDuration *time.Duration,
	tranItems ...*DynamoDBTransactionWrites) (success bool, err *DynamoDBError) {

	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 10 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	} else if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}

	if success, err = d.TransactionWriteItems(util.DurationPtr(timeout), tranItems...); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("TransactionWriteItemsWithRetry Failed: " + err.ErrorMessage)
				return d.TransactionWriteItemsWithRetry(maxRetries-1, util.DurationPtr(timeout), tranItems...)
			} else {
				if err.SuppressError {
					log.Println("TransactionWriteItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return false, nil
				} else {
					return false, &DynamoDBError{
						ErrorMessage:                      "TransactionWriteItemsWithRetry Failed: " + err.ErrorMessage,
						SuppressError:                     false,
						AllowRetry:                        false,
						RetryNeedsBackOff:                 false,
						TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("TransactionWriteItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return false, nil
			} else {
				return false, &DynamoDBError{
					ErrorMessage:                      "TransactionWriteItemsWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:                     false,
					AllowRetry:                        false,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
				}
			}
		}
	} else {
		// no error
		return success, nil
	}
}

// =====================================================================================================================
// TransactionGetItems Functions
// =====================================================================================================================

// TransactionGetItems receives parameters via GetItems Reads variadic objects of type DynamoDBTransactionReads; each object has TableName override in case querying against other tables
// Each SearchKeys struct object can contain one or more DynamoDBTableKeys struct, which contains PK, SK fields, and ResultItemsSlicePtr.
//
// The PK (required) and SK (optional) is used for search, while ResultItemsSlicePtr interface{} receives pointer to the output slice object,
// so that once query completes the appropriate item data will unmarshal into object
//
// important
//
//	if dynamodb table is defined as PK and SK together, then to search, MUST use PK and SK together or error will trigger
//
// setting result items slice ptr info
//  1. In the external calling code, must define slice of struct object pointers to receive such unmarshaled results
//     a) output := []*MID{
//     &MID{},
//     &MID{},
//     }
//     b) Usage
//     Passing each element of output to ResultItemsSlicePtr for the target scope of data
//
// notes:
//  1. getItems must contain at least one object
//  2. within getItems object, at least one object of DynamoDBTableKeyValue must exist for search
//  3. no more than total of 25 search keys allowed across all variadic objects
//  4. the ResultItemsSlicePtr in all getItems Reads objects within all variadic objects MUST BE SET
func (d *DynamoDB) TransactionGetItems(timeOutDuration *time.Duration, getItems ...*DynamoDBTransactionReads) (successCount int, err *DynamoDBError) {
	if xray.XRayServiceOn() {
		return d.transactionGetItemsWithTrace(timeOutDuration, getItems...)
	} else {
		return d.transactionGetItemsNormal(timeOutDuration, getItems...)
	}
}

func (d *DynamoDB) transactionGetItemsWithTrace(timeOutDuration *time.Duration, getItems ...*DynamoDBTransactionReads) (successCount int, err *DynamoDBError) {
	trace := xray.NewSegment("DynamoDB-TransactionGetItems", d._parentSegment)

	defer trace.Close()
	defer func() {
		if err != nil {
			_ = trace.Seg.AddError(fmt.Errorf(err.ErrorMessage))
		}
	}()

	if d.cn == nil {
		err = d.handleError(errors.New("DynamoDB Connection is Required"))
		return 0, err
	}

	if util.LenTrim(d.TableName) <= 0 {
		err = d.handleError(errors.New("DynamoDB Table Name is Required"))
		return 0, err
	}

	if util.LenTrim(d.PKName) <= 0 {
		err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "PK Name is Required"))
		return 0, err
	}

	if getItems == nil {
		err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems is Nil"))
		return 0, err
	}

	if len(getItems) <= 0 {
		err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems is Empty"))
		return 0, err
	}

	searchCount := 0

	for i := 0; i < len(getItems); i++ {
		if getItems[i] == nil {
			err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] Element is Nil"))
			return 0, err
		} else {
			if getItems[i].SearchKeys == nil {
				err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] SearchKeys is Nil"))
				return 0, err
			}

			if len(getItems[i].SearchKeys) <= 0 {
				err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] SearchKeys is Empty"))
				return 0, err
			}

			if getItems[i].ResultItemsSlicePtr == nil {
				err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] ResultItemsSlicePtr is Nil"))
				return 0, err
			}

			if reflect.TypeOf(getItems[i].ResultItemsSlicePtr).Kind() != reflect.Ptr {
				err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
				return 0, err
			}

			if reflect.ValueOf(getItems[i].ResultItemsSlicePtr).Elem().Kind() != reflect.Slice {
				err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
				return 0, err
			}

			searchCount += len(getItems[i].SearchKeys)

			if util.LenTrim(getItems[i].TableName) == 0 {
				getItems[i].TableName = d.TableName
				getItems[i].PKName = d.PKName
				getItems[i].SKName = d.SKName
			} else {
				if strings.ToUpper(getItems[i].TableName) == strings.ToUpper(d.TableName) {
					if util.LenTrim(getItems[i].PKName) == 0 {
						getItems[i].PKName = d.PKName
					}

					if util.LenTrim(getItems[i].SKName) == 0 {
						getItems[i].SKName = d.SKName
					}
				} else {
					if util.LenTrim(getItems[i].PKName) == 0 {
						getItems[i].PKName = "PK" // default
					}

					if util.LenTrim(getItems[i].SKName) == 0 {
						getItems[i].SKName = "SK" // default, might not be used, actual code decides at a later point
					}
				}
			}
		}
	}

	// search count must not exceed 25
	if searchCount > 25 {
		err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: (Validate Search Count) " + "Search Count May Not Exceed 25"))
		return 0, err
	}

	if searchCount <= 0 {
		err = d.handleError(errors.New("DynamoDB TransactionGetItems Failed: (Validate Search Count) " + "Search Count Minimum of 1 is Required"))
		return 0, err
	}

	trace.Capture("TransactionGetItems", func() error {
		//
		// prepare transaction get items request
		//
		transGetItems := make([]*dynamodb.TransactGetItem, 0)

		for _, searchSet := range getItems {
			//
			// marshal attributes key map from search set
			//
			if keysAv, keysErr := searchSet.MarshalSearchKeyValueMaps(); keysErr != nil {
				successCount = 0
				err = d.handleError(keysErr, "DynamoDB TransactionGetItems Failed: (SearchKey Marshal)")
				return fmt.Errorf(err.ErrorMessage)
			} else {
				// get projection expression and attribute names
				var projExpr *string
				var projAttr map[string]*string
				var projErr error

				if searchSet.ProjectedAttributes != nil {
					if projExpr, projAttr, projErr = searchSet.ProjectedAttributes.BuildProjectionParameters(); projErr != nil {
						successCount = 0
						err = d.handleError(projErr, "DynamoDB TransactionGetItems Failed: (Projecting Attributes)")
						return err
					}
				}

				// assign keys to TransGetItems
				for _, key := range keysAv {
					getItem := &dynamodb.Get{
						TableName: aws.String(searchSet.TableName),
						Key:       key,
					}

					if projExpr != nil && util.LenTrim(*projExpr) > 0 && projAttr != nil && len(projAttr) > 0 {
						getItem.ProjectionExpression = projExpr
						getItem.ExpressionAttributeNames = projAttr
					}

					transGetItems = append(transGetItems, &dynamodb.TransactGetItem{
						Get: getItem,
					})
				}
			}
		}

		// compose transaction get items input var
		params := &dynamodb.TransactGetItemsInput{
			TransactItems: transGetItems,
		}

		// record params payload
		d.LastExecuteParamsPayload = "TransactionGetItems = " + params.String()

		// execute transaction get operation
		var result *dynamodb.TransactGetItemsOutput
		var err1 error

		subTrace := trace.NewSubSegment("TransactionGetItems_Do")
		defer subTrace.Close()

		if timeOutDuration != nil {
			ctx, cancel := context.WithTimeout(subTrace.Ctx, *timeOutDuration)
			defer cancel()
			result, err1 = d.do_TransactGetItems(params, ctx)
		} else {
			result, err1 = d.do_TransactGetItems(params, subTrace.Ctx)
		}

		if err1 != nil {
			successCount = 0
			err = d.handleError(err1, "DynamoDB TransactionGetItems Failed: (Transaction Reads)")
			return err
		}

		// evaluate response
		successCount = 0

		if result.Responses != nil && len(result.Responses) > 0 {
			//
			// loop through each searchSet to assign Result Items from Responses
			//
			for _, searchSet := range getItems {
				if searchSet != nil {
					if respErr := searchSet.UnmarshalResultItems(result.Responses); respErr != nil {
						successCount = 0
						err = d.handleError(respErr, "DynamoDB TransactionGetItems Failed: (Unmarshal Result)")
						return err
					} else {
						successCount += searchSet.ResultItemsCount
					}
				}
			}
		}

		err = nil
		return nil
	}, &xray.XTraceData{
		Meta: map[string]interface{}{
			"TableName": d.TableName,
			"GetItems":  getItems,
		},
	})

	// nothing found or something found, both returns nil for error
	return successCount, err
}

func (d *DynamoDB) transactionGetItemsNormal(timeOutDuration *time.Duration, getItems ...*DynamoDBTransactionReads) (successCount int, err *DynamoDBError) {
	if d.cn == nil {
		return 0, d.handleError(errors.New("DynamoDB Connection is Required"))
	}

	if util.LenTrim(d.TableName) <= 0 {
		return 0, d.handleError(errors.New("DynamoDB Table Name is Required"))
	}

	if util.LenTrim(d.PKName) <= 0 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "PK Name is Required"))
	}

	if getItems == nil {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems is Nil"))
	}

	if len(getItems) <= 0 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems is Empty"))
	}

	searchCount := 0

	for i := 0; i < len(getItems); i++ {
		if getItems[i] == nil {
			return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] Element is Nil"))
		} else {
			if getItems[i].SearchKeys == nil {
				return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] SearchKeys is Nil"))
			}

			if len(getItems[i].SearchKeys) <= 0 {
				return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] SearchKeys is Empty"))
			}

			if getItems[i].ResultItemsSlicePtr == nil {
				return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] ResultItemsSlicePtr is Nil"))
			}

			if reflect.TypeOf(getItems[i].ResultItemsSlicePtr).Kind() != reflect.Ptr {
				return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
			}

			if reflect.ValueOf(getItems[i].ResultItemsSlicePtr).Elem().Kind() != reflect.Slice {
				return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: " + "GetItems[" + util.Itoa(i) + "] ResultItemsSlicePtr Expected To Be Slice Pointer"))
			}

			searchCount += len(getItems[i].SearchKeys)

			if util.LenTrim(getItems[i].TableName) == 0 {
				getItems[i].TableName = d.TableName
				getItems[i].PKName = d.PKName
				getItems[i].SKName = d.SKName
			} else {
				if strings.ToUpper(getItems[i].TableName) == strings.ToUpper(d.TableName) {
					if util.LenTrim(getItems[i].PKName) == 0 {
						getItems[i].PKName = d.PKName
					}

					if util.LenTrim(getItems[i].SKName) == 0 {
						getItems[i].SKName = d.SKName
					}
				} else {
					if util.LenTrim(getItems[i].PKName) == 0 {
						getItems[i].PKName = "PK" // default
					}

					if util.LenTrim(getItems[i].SKName) == 0 {
						getItems[i].SKName = "SK" // default, might not be used, actual code decides at a later point
					}
				}
			}
		}
	}

	// search count must not exceed 25
	if searchCount > 25 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: (Validate Search Count) " + "Search Count May Not Exceed 25"))
	}

	if searchCount <= 0 {
		return 0, d.handleError(errors.New("DynamoDB TransactionGetItems Failed: (Validate Search Count) " + "Search Count Minimum of 1 is Required"))
	}

	//
	// prepare transaction get items request
	//
	transGetItems := make([]*dynamodb.TransactGetItem, 0)

	for _, searchSet := range getItems {
		//
		// marshal attributes key map from search set
		//
		if keysAv, keysErr := searchSet.MarshalSearchKeyValueMaps(); keysErr != nil {
			return 0, d.handleError(keysErr, "DynamoDB TransactionGetItems Failed: (SearchKey Marshal)")
		} else {
			// get projection expression and attribute names
			var projExpr *string
			var projAttr map[string]*string
			var projErr error

			if searchSet.ProjectedAttributes != nil {
				if projExpr, projAttr, projErr = searchSet.ProjectedAttributes.BuildProjectionParameters(); projErr != nil {
					return 0, d.handleError(projErr, "DynamoDB TransactionGetItems Failed: (Projecting Attributes)")
				}
			}

			// assign keys to TransGetItems
			for _, key := range keysAv {
				getItem := &dynamodb.Get{
					TableName: aws.String(searchSet.TableName),
					Key:       key,
				}

				if projExpr != nil && util.LenTrim(*projExpr) > 0 && projAttr != nil && len(projAttr) > 0 {
					getItem.ProjectionExpression = projExpr
					getItem.ExpressionAttributeNames = projAttr
				}

				transGetItems = append(transGetItems, &dynamodb.TransactGetItem{
					Get: getItem,
				})
			}
		}
	}

	// compose transaction get items input var
	params := &dynamodb.TransactGetItemsInput{
		TransactItems: transGetItems,
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
		//
		// loop through each searchSet to assign Result Items from Responses
		//
		for _, searchSet := range getItems {
			if searchSet != nil {
				if respErr := searchSet.UnmarshalResultItems(result.Responses); respErr != nil {
					return 0, d.handleError(respErr, "DynamoDB TransactionGetItems Failed: (Unmarshal Result)")
				} else {
					successCount += searchSet.ResultItemsCount
				}
			}
		}
	}

	// nothing found or something found, both returns nil for error
	return successCount, nil
}

// TransactionGetItemsWithRetry handles dynamodb retries in case action temporarily fails
func (d *DynamoDB) TransactionGetItemsWithRetry(maxRetries uint,
	timeOutDuration *time.Duration,
	getItems ...*DynamoDBTransactionReads) (successCount int, err *DynamoDBError) {

	if maxRetries > 10 {
		maxRetries = 10
	}

	timeout := 5 * time.Second

	if timeOutDuration != nil {
		timeout = *timeOutDuration
	}

	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	} else if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	if successCount, err = d.TransactionGetItems(util.DurationPtr(timeout), getItems...); err != nil {
		// has error
		if maxRetries > 0 {
			if err.AllowRetry {
				if err.RetryNeedsBackOff {
					time.Sleep(500 * time.Millisecond)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				log.Println("TransactionGetItemsWithRetry Failed: " + err.ErrorMessage)
				return d.TransactionGetItemsWithRetry(maxRetries-1, util.DurationPtr(timeout), getItems...)
			} else {
				if err.SuppressError {
					log.Println("TransactionGetItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = " + util.UintToStr(maxRetries) + ")")
					return 0, nil
				} else {
					return 0, &DynamoDBError{
						ErrorMessage:                      "TransactionGetItemsWithRetry Failed: " + err.ErrorMessage,
						SuppressError:                     false,
						AllowRetry:                        false,
						RetryNeedsBackOff:                 false,
						TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
					}
				}
			}
		} else {
			if err.SuppressError {
				log.Println("TransactionGetItemsWithRetry DynamoDB Error Suppressed, Returning Error Nil (MaxRetries = 0)")
				return 0, nil
			} else {
				return 0, &DynamoDBError{
					ErrorMessage:                      "TransactionGetItemsWithRetry Failed: (MaxRetries = 0) " + err.ErrorMessage,
					SuppressError:                     false,
					AllowRetry:                        false,
					RetryNeedsBackOff:                 false,
					TransactionConditionalCheckFailed: err.TransactionConditionalCheckFailed,
				}
			}
		}
	} else {
		// no error
		return successCount, nil
	}
}

// *********************************************************************************************************************
// *********************************************************************************************************************
// *********************************************************************************************************************
//
// DYNAMODB TABLE DEFINITION UTILITIES
//
// *********************************************************************************************************************
// *********************************************************************************************************************
// *********************************************************************************************************************

// =====================================================================================================================
// CreateTable Utility Function
// =====================================================================================================================

// CreateTable creates a new dynamodb table to the default aws region (as configured by aws cli)
func (d *DynamoDB) CreateTable(input *dynamodb.CreateTableInput, ctx ...aws.Context) (*dynamodb.CreateTableOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB CreateTable Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB CreateTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.CreateTable(input)
	} else {
		return d.cn.CreateTableWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// UpdateTable Utility Function
// =====================================================================================================================

// UpdateTable updates an existing dynamodb table with provided input parameter
func (d *DynamoDB) UpdateTable(input *dynamodb.UpdateTableInput, ctx ...aws.Context) (*dynamodb.UpdateTableOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB UpdateTable Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB UpdateTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.UpdateTable(input)
	} else {
		return d.cn.UpdateTableWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// DeleteTable Utility Function
// =====================================================================================================================

// DeleteTable deletes an existing dynamodb table
func (d *DynamoDB) DeleteTable(input *dynamodb.DeleteTableInput, ctx ...aws.Context) (*dynamodb.DeleteTableOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB DeleteTable Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB DeleteTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.DeleteTable(input)
	} else {
		return d.cn.DeleteTableWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// ListTables Utility Function
// =====================================================================================================================

// ListTables queries dynamodb tables list and returns found tables info
func (d *DynamoDB) ListTables(input *dynamodb.ListTablesInput, ctx ...aws.Context) (*dynamodb.ListTablesOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB ListTables Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB ListTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.ListTables(input)
	} else {
		return d.cn.ListTablesWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// DescribeTable Utility Function
// =====================================================================================================================

// DescribeTable describes the dynamodb table info for target identified in input parameter
func (d *DynamoDB) DescribeTable(input *dynamodb.DescribeTableInput, ctx ...aws.Context) (*dynamodb.DescribeTableOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB DescribeTable Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB DescribeTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.DescribeTable(input)
	} else {
		return d.cn.DescribeTableWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// CreateGlobalTable Utility Function
// =====================================================================================================================

// CreateGlobalTable creates a dynamodb global table
func (d *DynamoDB) CreateGlobalTable(input *dynamodb.CreateGlobalTableInput, ctx ...aws.Context) (*dynamodb.CreateGlobalTableOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB CreateGlobalTable Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB CreateGlobalTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.CreateGlobalTable(input)
	} else {
		return d.cn.CreateGlobalTableWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// UpdateGlobalTable Utility Function
// =====================================================================================================================

// UpdateGlobalTable updates a dynamodb global table
func (d *DynamoDB) UpdateGlobalTable(input *dynamodb.UpdateGlobalTableInput, ctx ...aws.Context) (*dynamodb.UpdateGlobalTableOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB UpdateGlobalTable Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB UpdateGlobalTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.UpdateGlobalTable(input)
	} else {
		return d.cn.UpdateGlobalTableWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// ListGlobalTables Utility Function
// =====================================================================================================================

// ListGlobalTables lists dynamodb global tables
func (d *DynamoDB) ListGlobalTables(input *dynamodb.ListGlobalTablesInput, ctx ...aws.Context) (*dynamodb.ListGlobalTablesOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB ListGlobalTables Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB ListGlobalTables Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.ListGlobalTables(input)
	} else {
		return d.cn.ListGlobalTablesWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// DescribeGlobalTable Utility Function
// =====================================================================================================================

// DescribeGlobalTable describes dynamodb global table
func (d *DynamoDB) DescribeGlobalTable(input *dynamodb.DescribeGlobalTableInput, ctx ...aws.Context) (*dynamodb.DescribeGlobalTableOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB DescribeGlobalTable Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB DescribeGlobalTable Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.DescribeGlobalTable(input)
	} else {
		return d.cn.DescribeGlobalTableWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// CreateBackup Utility Function
// =====================================================================================================================

// CreateBackup creates dynamodb table backup
func (d *DynamoDB) CreateBackup(input *dynamodb.CreateBackupInput, ctx ...aws.Context) (*dynamodb.CreateBackupOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB CreateBackup Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB CreateBackup Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.CreateBackup(input)
	} else {
		return d.cn.CreateBackupWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// DeleteBackup Utility Function
// =====================================================================================================================

// DeleteBackup deletes an existing dynamodb table backup
func (d *DynamoDB) DeleteBackup(input *dynamodb.DeleteBackupInput, ctx ...aws.Context) (*dynamodb.DeleteBackupOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB DeleteBackup Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB DeleteBackup Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.DeleteBackup(input)
	} else {
		return d.cn.DeleteBackupWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// ListBackups Utility Function
// =====================================================================================================================

// ListBackups lists dynamodb table backup
func (d *DynamoDB) ListBackups(input *dynamodb.ListBackupsInput, ctx ...aws.Context) (*dynamodb.ListBackupsOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB ListBackups Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB ListBackups Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.ListBackups(input)
	} else {
		return d.cn.ListBackupsWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// DescribeBackup Utility Function
// =====================================================================================================================

// DescribeBackup describes dynamodb table backup
func (d *DynamoDB) DescribeBackup(input *dynamodb.DescribeBackupInput, ctx ...aws.Context) (*dynamodb.DescribeBackupOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB DescribeBackup Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB DescribeBackup Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.DescribeBackup(input)
	} else {
		return d.cn.DescribeBackupWithContext(ctx[0], input)
	}
}

// =====================================================================================================================
// UpdatePointInTimeBackup Utility Function
// =====================================================================================================================

// UpdatePointInTimeBackup updates dynamodb table point in time backup option
func (d *DynamoDB) UpdatePointInTimeBackup(input *dynamodb.UpdateContinuousBackupsInput, ctx ...aws.Context) (*dynamodb.UpdateContinuousBackupsOutput, error) {
	if d.cn == nil {
		return nil, fmt.Errorf("DynamoDB UpdatePointInTimeBackup Failed: " + "No DynamoDB Connection Available")
	}

	if input == nil {
		return nil, fmt.Errorf("DynamoDB UpdatePointInTimeBackup Failed: " + "Input Object is Required")
	}

	if len(ctx) <= 0 {
		return d.cn.UpdateContinuousBackups(input)
	} else {
		return d.cn.UpdateContinuousBackupsWithContext(ctx[0], input)
	}
}
