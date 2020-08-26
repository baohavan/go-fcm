package fcm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	firebase "firebase.google.com/go"
	"firebase.google.com/go/messaging"
	"fmt"
	"google.golang.org/api/option"
	"net/http"
	"time"
)

const (
	// DefaultEndpoint contains endpoint URL of FCM service.
	DefaultEndpoint = "https://fcm.googleapis.com/fcm/send"

	// DefaultTimeout duration in second
	DefaultTimeout time.Duration = 30 * time.Second
)

var (
	// ErrInvalidAPIKey occurs if API key is not set.
	ErrInvalidAPIKey          = errors.New("client API Key is invalid")
	ErrInvalidCredentialsPath = errors.New("credentials path is invalid")
)

// Client abstracts the interaction between the application server and the
// FCM server via HTTP protocol. The developer must obtain an API key from the
// Google APIs Console page and pass it to the `Client` so that it can
// perform authorized requests on the application server's behalf.
// To send a message to one or more devices use the Client's Send.
//
// If the `HTTP` field is nil, a zeroed http.Client will be allocated and used
// to send messages.
type Client struct {
	apiKey    string
	client    *http.Client
	endpoint  string
	timeout   time.Duration
	app       *firebase.App
	msgClient *messaging.Client
}

// NewClient creates new Firebase Cloud Messaging Client based on API key and
// with default endpoint and http client.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, ErrInvalidAPIKey
	}
	c := &Client{
		apiKey:   apiKey,
		endpoint: DefaultEndpoint,
		client:   &http.Client{},
		timeout:  DefaultTimeout,
		app:      nil,
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// NewClient creates new Firebase Cloud Messaging Client based on Credentials file and
// with default endpoint and http client.
func NewClientWithCredentials(path string, opts ...Option) (*Client, error) {
	if path == "" {
		return nil, ErrInvalidCredentialsPath
	}

	ctx := context.Background()
	app, err := firebase.NewApp(ctx, nil, option.WithCredentialsFile(path))
	if err != nil {
		return nil, err
	}

	c := &Client{
		endpoint:  DefaultEndpoint,
		client:    nil,
		timeout:   DefaultTimeout,
		app:       app,
		msgClient: nil,
	}

	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// SendWithContext sends a message to the FCM server without retrying in case of service
// unavailability. A non-nil error is returned if a non-recoverable error
// occurs (i.e. if the response status is not "200 OK").
// Behaves just like regular send, but uses external context.
func (c *Client) SendWithContext(ctx context.Context, msg *Message) (*Response, error) {
	// validate
	if err := msg.Validate(); err != nil {
		return nil, err
	}

	if c.app != nil {
		return c.sendByApp(ctx, msg)
	} else {
		// marshal message
		data, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}

		return c.send(ctx, data)
	}
}

// Send sends a message to the FCM server without retrying in case of service
// unavailability. A non-nil error is returned if a non-recoverable error
// occurs (i.e. if the response status is not "200 OK").
func (c *Client) Send(msg *Message) (*Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	return c.SendWithContext(ctx, msg)
}

// SendWithRetry sends a message to the FCM server with defined number of
// retrying in case of temporary error.
func (c *Client) SendWithRetry(msg *Message, retryAttempts int) (*Response, error) {
	return c.SendWithRetryWithContext(context.Background(), msg, retryAttempts)
}

// SendWithRetryWithContext sends a message to the FCM server with defined number of
// retrying in case of temporary error.
// Behaves just like regular SendWithRetry, but uses external context.
func (c *Client) SendWithRetryWithContext(ctx context.Context, msg *Message, retryAttempts int) (*Response, error) {
	// validate
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	// marshal message
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	resp := new(Response)
	err = retry(func() error {
		ctx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel()
		var er error
		resp, er = c.send(ctx, data)
		return er
	}, retryAttempts)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// send sends a request.
func (c *Client) send(ctx context.Context, data []byte) (*Response, error) {
	// create request
	req, err := http.NewRequest("POST", c.endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}

	req = req.WithContext(ctx)

	// add headers
	req.Header.Add("Authorization", fmt.Sprintf("key=%s", c.apiKey))
	req.Header.Add("Content-Type", "application/json")

	// execute request
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, connectionError(err.Error())
	}
	defer resp.Body.Close()

	// check response status
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError {
			return nil, serverError(fmt.Sprintf("%d error: %s", resp.StatusCode, resp.Status))
		}
		return nil, fmt.Errorf("%d error: %s", resp.StatusCode, resp.Status)
	}

	// build return
	response := new(Response)
	if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
		return nil, err
	}

	return response, nil
}

// send by app
func (c *Client) sendByApp(ctx context.Context, msg *Message) (*Response, error) {
	if c.msgClient == nil {
		var err error
		c.msgClient, err = c.app.Messaging(ctx)
		if err != nil {
			return nil, err
		}
	}

	data := make(map[string]string)
	for k, v := range msg.Data {
		data[k] = v.(string)
	}
	if len(msg.RegistrationIDs) > 0 && len(msg.RegistrationIDs) < 2 {
		fcmMsg := &messaging.Message{
			Token: msg.RegistrationIDs[0],
			Data:  data,
		}

		if msg.Notification != nil {
			fcmMsg.Notification = &messaging.Notification{
				Title: msg.Notification.Title,
				Body:  msg.Notification.Body,
			}
		}

		r, err := c.msgClient.Send(ctx, fcmMsg)
		res := &Response{}
		if err != nil {
			res.Success = 0
			res.Failure = 1
		} else {
			res.Success = 1
			res.Failure = 0
		}
		res.Error = err

		res.ErrorResponseCode = r

		return res, err
	} else {
		fcmMsg := &messaging.MulticastMessage{
			Tokens: msg.RegistrationIDs,
			Data:   data,
		}

		if msg.Notification != nil {
			fcmMsg.Notification = &messaging.Notification{
				Title: msg.Notification.Title,
				Body:  msg.Notification.Body,
			}
		}

		r, err := c.msgClient.SendMulticast(ctx, fcmMsg)
		res := &Response{}
		if err != nil {
			res.Success = 0
			res.Failure = len(msg.RegistrationIDs)
		} else {
			res.Success = r.SuccessCount
			res.Failure = 0
		}

		for _, result := range r.Responses {
			if result.Error == nil {
				res.Results = append(res.Results, Result{
					Error:     result.Error,
					MessageID: result.MessageID,
				})
			} else {
				res.Results = append(res.Results, Result{
					Error:             result.Error,
					ErrorResponseCode: result.Error.Error(),
				})
			}
		}

		return res, err
	}
}
