package chunk

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

type mockDynamoDBClient struct {
	dynamodbiface.DynamoDBAPI

	mtx            sync.RWMutex
	unprocessed    int
	provisionedErr int
	tables         map[string]*mockDynamoDBTable
}

type mockDynamoDBTable struct {
	items map[string][]mockDynamoDBItem
}

type mockDynamoDBItem map[string]*dynamodb.AttributeValue

func newMockDynamoDB(unprocessed int, provisionedErr int) *mockDynamoDBClient {
	return &mockDynamoDBClient{
		tables:         map[string]*mockDynamoDBTable{},
		unprocessed:    unprocessed,
		provisionedErr: provisionedErr,
	}
}

func (m *mockDynamoDBClient) createTable(name string) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.tables[name] = &mockDynamoDBTable{
		items: map[string][]mockDynamoDBItem{},
	}
}

func (m *mockDynamoDBClient) BatchWriteItem(input *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	resp := &dynamodb.BatchWriteItemOutput{
		UnprocessedItems: map[string][]*dynamodb.WriteRequest{},
	}

	if m.provisionedErr > 0 {
		m.provisionedErr--
		return resp, awserr.New(provisionedThroughputExceededException, "", nil)
	}

	for tableName, writeRequests := range input.RequestItems {
		table, ok := m.tables[tableName]
		if !ok {
			return &dynamodb.BatchWriteItemOutput{}, fmt.Errorf("table not found")
		}

		for _, writeRequest := range writeRequests {
			if m.unprocessed > 0 {
				m.unprocessed--
				resp.UnprocessedItems[tableName] = append(resp.UnprocessedItems[tableName], writeRequest)
				continue
			}

			hashValue := *writeRequest.PutRequest.Item[hashKey].S
			rangeValue := writeRequest.PutRequest.Item[rangeKey].B

			items := table.items[hashValue]

			// insert in order
			i := sort.Search(len(items), func(i int) bool {
				return bytes.Compare(items[i][rangeKey].B, rangeValue) >= 0
			})
			if i >= len(items) || !bytes.Equal(items[i][rangeKey].B, rangeValue) {
				items = append(items, nil)
				copy(items[i+1:], items[i:])
			} else {
				return &dynamodb.BatchWriteItemOutput{}, fmt.Errorf("Duplicate entry")
			}
			items[i] = writeRequest.PutRequest.Item

			table.items[hashValue] = items
		}
	}
	return resp, nil
}

func TestDynamoDBClient(t *testing.T) {
	dynamoDB := newMockDynamoDB(0, 0)
	client := awsStorageClient{
		DynamoDB: dynamoDB,
	}
	batch := client.NewWriteBatch()
	for i := 0; i < 30; i++ {
		batch.Add("table", fmt.Sprintf("hash%d", i), []byte(fmt.Sprintf("range%d", i)), nil)
	}
	dynamoDB.createTable("table")

	if err := client.BatchWrite(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
}

func TestAWSConfigFromURL(t *testing.T) {
	for _, tc := range []struct {
		url            string
		expectedKey    string
		expectedSecret string
		expectedRegion string
		expectedEp     string

		expectedNotSpecifiedUserErr bool
	}{
		{
			"s3://abc:123@s3.default.svc.cluster.local:4569",
			"abc",
			"123",
			"dummy",
			"http://s3.default.svc.cluster.local:4569",
			false,
		},
		{
			"dynamodb://user:pass@dynamodb.default.svc.cluster.local:8000/cortex",
			"user",
			"pass",
			"dummy",
			"http://dynamodb.default.svc.cluster.local:8000",
			false,
		},
		{
			// Not escaped password.
			"s3://abc:123/@s3.default.svc.cluster.local:4569",
			"",
			"",
			"",
			"",
			true,
		},
		{
			// Not escaped username.
			"s3://abc/:123@s3.default.svc.cluster.local:4569",
			"",
			"",
			"",
			"",
			true,
		},
		{
			"s3://keyWithEscapedSlashAtTheEnd%2F:%24%2C%26%2C%2B%2C%27%2C%2F%2C%3A%2C%3B%2C%3D%2C%3F%2C%40@eu-west-2/bucket1",
			"keyWithEscapedSlashAtTheEnd/",
			"$,&,+,',/,:,;,=,?,@",
			"eu-west-2",
			"",
			false,
		},
	} {
		parsedURL, err := url.Parse(tc.url)
		require.NoError(t, err)

		cfg, err := awsConfigFromURL(parsedURL)
		if tc.expectedNotSpecifiedUserErr {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)

		require.NotNil(t, cfg.Credentials)
		val, err := cfg.Credentials.Get()
		require.NoError(t, err)

		assert.Equal(t, tc.expectedKey, val.AccessKeyID)
		assert.Equal(t, tc.expectedSecret, val.SecretAccessKey)

		require.NotNil(t, cfg.Region)
		assert.Equal(t, tc.expectedRegion, *cfg.Region)

		if tc.expectedEp != "" {
			require.NotNil(t, cfg.Endpoint)
			assert.Equal(t, tc.expectedEp, *cfg.Endpoint)
		}
	}
}
