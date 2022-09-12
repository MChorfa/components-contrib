/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/dapr/components-contrib/configuration"
	"github.com/dapr/kit/logger"
	"github.com/google/uuid"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4/pgxpool"
	_ "github.com/jackc/pgx/v4/stdlib"
	"golang.org/x/exp/utf8string"
)

type ConfigurationStore struct {
	metadata             metadata
	client               *pgxpool.Pool
	logger               logger.Logger
	subscribeStopChanMap sync.Map
}

const (
	configtablekey               = "table"
	connMaxIdleTimeKey           = "connMaxIdleTime"
	connectionStringKey          = "connectionString"
	ErrorMissingTableName        = "missing postgreSQL configuration table name"
	InfoStartInit                = "Initializing PostgreSQL state store"
	ErrorMissingConnectionString = "missing postgreSQL connection string"
	ErrorAlreadyInitialized      = "PostgreSQL configuration store already initialized"
	ErrorMissinMaxTimeout        = "missing PostgreSQL maxTimeout setting in configuration"
	QueryTableExists             = "SELECT EXISTS (SELECT FROM pg_tables where tablename = $1)"
	maxIdentifierLength          = 64 // https://www.postgresql.org/docs/current/limits.html
	ErrorTooLongFieldLength      = "field name is too long"
)

func NewPostgresConfigurationStore(logger logger.Logger) configuration.Store {
	logger.Debug("Instantiating PostgreSQL configuration store")
	return &ConfigurationStore{
		logger: logger,
	}
}

func (p *ConfigurationStore) Init(metadata configuration.Metadata) error {
	p.logger.Debug(InfoStartInit)
	if p.client != nil {
		return fmt.Errorf(ErrorAlreadyInitialized)
	}
	if m, err := parseMetadata(metadata); err != nil {
		p.logger.Error(err)
		return err
	} else {
		p.metadata = m
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.metadata.maxIdleTime)
	defer cancel()
	client, err := Connect(ctx, p.metadata.connectionString, p.metadata.maxIdleTime)
	if err != nil {
		return err
	}
	p.client = client
	pingErr := p.client.Ping(ctx)
	if pingErr != nil {
		return pingErr
	}

	// check if table exists
	exists := false
	err = p.client.QueryRow(ctx, QueryTableExists, p.metadata.configTable).Scan(&exists)
	if err != nil {
		return err
	}
	return nil
}

func (p *ConfigurationStore) Get(ctx context.Context, req *configuration.GetRequest) (*configuration.GetResponse, error) {
	query, err := buildQuery(req, p.metadata.configTable)
	if err != nil {
		p.logger.Error(err)
		return nil, err
	}

	rows, err := p.client.Query(ctx, query)
	if err != nil {
		// If no rows exist, return an empty response, otherwise return the error.
		if err == sql.ErrNoRows {
			return &configuration.GetResponse{}, nil
		}
		return nil, err
	}
	response := configuration.GetResponse{}
	for i := 0; rows.Next(); i++ {
		var item configuration.Item
		var key string
		var metadata []byte
		v := make(map[string]string)

		if err := rows.Scan(key, &item.Value, &item.Version, &metadata); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metadata, &v); err != nil {
			return nil, err
		}
		item.Metadata = v
		response.Items[key] = &item
	}
	return &response, nil
}

func (p *ConfigurationStore) Subscribe(ctx context.Context, req *configuration.SubscribeRequest, handler configuration.UpdateHandler) (string, error) {
	subscribeID := uuid.New().String()
	key := "listen " + p.metadata.configTable
	// subscribe to events raised on the configTable
	if oldStopChan, ok := p.subscribeStopChanMap.Load(key); ok {
		close(oldStopChan.(chan struct{}))
	}
	stop := make(chan struct{})
	p.subscribeStopChanMap.Store(subscribeID, stop)
	go p.doSubscribe(ctx, req, handler, key, subscribeID, stop)
	return subscribeID, nil
}

func (p *ConfigurationStore) Unsubscribe(ctx context.Context, req *configuration.UnsubscribeRequest) error {
	if oldStopChan, ok := p.subscribeStopChanMap.Load(req.ID); ok {
		p.subscribeStopChanMap.Delete(req.ID)
		close(oldStopChan.(chan struct{}))
	}
	return nil
}

func (p *ConfigurationStore) doSubscribe(ctx context.Context, req *configuration.SubscribeRequest, handler configuration.UpdateHandler, channel string, id string, stop chan struct{}) {
	conn, err := p.client.Acquire(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error acquiring connection:", err)
	}
	defer conn.Release()
	ctxTimeout, cancel := context.WithTimeout(ctx, p.metadata.maxIdleTime)
	defer cancel()
	_, err = conn.Exec(ctxTimeout, channel)
	if err != nil {
		p.logger.Errorf("Error listening to channel:", err)
		return
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctxTimeout)
		if err != nil {
			if !(pgconn.Timeout(err) || errors.Is(ctxTimeout.Err(), context.Canceled)) {
				p.logger.Errorf("Error waiting for notification:", err)
			}
			return
		}
		p.handleSubscribedChange(ctx, handler, notification, id)
	}
}

func (p *ConfigurationStore) handleSubscribedChange(ctx context.Context, handler configuration.UpdateHandler, msg *pgconn.Notification, id string) {
	defer func() {
		if err := recover(); err != nil {
			p.logger.Errorf("panic in handleSubscribedChange method and recovered: %s", err)
		}
	}()
	payload := make(map[string]interface{})
	err := json.Unmarshal([]byte(msg.Payload), &payload)
	if err != nil {
		p.logger.Errorf("Error in UnMarshal: ", err)
		return
	}

	var key, value, version string
	m := make(map[string]string)
	// trigger should encapsulate the row in "data" field in the notification
	row := reflect.ValueOf(payload["data"])
	if row.Kind() == reflect.Map {
		for _, k := range row.MapKeys() {
			v := row.MapIndex(k)
			strKey := k.Interface().(string)
			switch strings.ToLower(strKey) {
			case "key":
				key = v.Interface().(string)
			case "value":
				value = v.Interface().(string)
			case "version":
				version = v.Interface().(string)
			case "metadata":
				a := v.Interface().(map[string]interface{})
				for k, v := range a {
					m[k] = v.(string)
				}
			}
		}
	}
	e := &configuration.UpdateEvent{
		Items: map[string]*configuration.Item{
			key: {
				Value:    value,
				Version:  version,
				Metadata: m,
			},
		},
		ID: id,
	}
	err = handler(ctx, e)
	if err != nil {
		p.logger.Errorf("fail to call handler to notify event for configuration update subscribe: %s", err)
	}
}

func parseMetadata(cmetadata configuration.Metadata) (metadata, error) {
	m := metadata{}

	if val, ok := cmetadata.Properties[connectionStringKey]; ok && val != "" {
		m.connectionString = val
	} else {
		return m, fmt.Errorf(ErrorMissingConnectionString)
	}

	if tbl, ok := cmetadata.Properties[configtablekey]; ok && tbl != "" {
		if !utf8string.NewString(tbl).IsASCII() {
			return m, fmt.Errorf("invalid table name : '%v'. non-ascii characters are not supported", tbl)
		}
		if len(tbl) > maxIdentifierLength {
			return m, fmt.Errorf(ErrorTooLongFieldLength+" - tableName : '%v'. max allowed field length is %v ", tbl, maxIdentifierLength)
		}
		m.configTable = tbl
	} else {
		return m, fmt.Errorf(ErrorMissingTableName)
	}

	// configure maxTimeout if provided
	if mxTimeout, ok := cmetadata.Properties[connMaxIdleTimeKey]; ok && mxTimeout != "" {
		if t, err := time.ParseDuration(mxTimeout); err == nil {
			m.maxIdleTime = t
		} else {
			return m, fmt.Errorf(ErrorMissinMaxTimeout)
		}
	}
	return m, nil
}

func Connect(ctx context.Context, conn string, maxTimeout time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.Connect(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("postgres configuration store connection error : %s", err)
	}
	pingErr := pool.Ping(ctx)
	if pingErr != nil {
		return nil, fmt.Errorf("postgres configuration store ping error : %s", pingErr)
	}
	return pool, nil
}

func buildQuery(req *configuration.GetRequest, configTable string) (string, error) {
	var query string
	if len(req.Keys) == 0 {
		query = "SELECT * FROM " + configTable
	} else {
		var queryBuilder strings.Builder
		queryBuilder.WriteString("SELECT * FROM " + configTable + " WHERE KEY IN ('")
		queryBuilder.WriteString(strings.Join(req.Keys, "','"))
		queryBuilder.WriteString("')")
		query = queryBuilder.String()
	}

	if len(req.Metadata) > 0 {
		var s strings.Builder
		i, j := len(req.Metadata), 0
		s.WriteString(" AND ")
		for k, v := range req.Metadata {
			temp := k + "='" + v + "'"
			s.WriteString(temp)
			if j++; j < i {
				s.WriteString(" AND ")
			}
		}
		query += s.String()
	}
	return query, nil
}

func QueryRow(ctx context.Context, p *pgxpool.Pool, query string, tbl string) error {
	exists := false
	err := p.QueryRow(ctx, query, tbl).Scan(&exists)
	if err != nil {
		return fmt.Errorf("postgres configuration store query error : %s", err)
	}
	return nil
}
