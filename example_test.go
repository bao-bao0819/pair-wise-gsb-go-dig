// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package dig_test

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"go.uber.org/dig"
)

func Example_minimal() {
	type Config struct {
		Prefix string
	}

	c := dig.New()

	// Provide a Config object. This can fail to decode.
	err := c.Provide(func() (*Config, error) {
		// In a real program, the configuration will probably be read from a
		// file.
		var cfg Config
		err := json.Unmarshal([]byte(`{"prefix": "[foo] "}`), &cfg)
		return &cfg, err
	})
	if err != nil {
		panic(err)
	}

	// Provide a way to build the logger based on the configuration.
	err = c.Provide(func(cfg *Config) *log.Logger {
		return log.New(os.Stdout, cfg.Prefix, 0)
	})
	if err != nil {
		panic(err)
	}

	// Invoke a function that requires the logger, which in turn builds the
	// Config first.
	err = c.Invoke(func(l *log.Logger) {
		l.Print("You've been invoked")
	})
	if err != nil {
		panic(err)
	}

	// Output:
	// [foo] You've been invoked
}

func Example_provideBatch() {
	type Config struct{ Name string }
	type Service struct{ Name string }

	c := dig.New()

	// A whole group of constructors is registered at once. The service
	// constructor may depend on a type produced by an earlier entry in the
	// same batch.
	err := c.ProvideBatch(
		dig.BatchProvider{
			Constructor: func() *Config { return &Config{Name: "myapp"} },
		},
		dig.BatchProvider{
			Constructor: func(cfg *Config) *Service {
				return &Service{Name: cfg.Name}
			},
			Options: []dig.ProvideOption{dig.Name("main")},
		},
	)
	if err != nil {
		panic(err)
	}

	type params struct {
		dig.In

		Service *Service `name:"main"`
	}
	if err := c.Invoke(func(p params) {
		fmt.Println(p.Service.Name)
	}); err != nil {
		panic(err)
	}

	// A batch that fails (here, a duplicate registration) changes nothing:
	// neither constructor is left behind in the container.
	err = c.ProvideBatch(
		dig.BatchProvider{Constructor: func() *Config { return &Config{} }},
		dig.BatchProvider{Constructor: func() *Config { return &Config{} }},
	)
	if err == nil {
		panic("expected duplicate registration to fail")
	}
	fmt.Println("failed batch left the container untouched")

	// Output:
	// myapp
	// failed batch left the container untouched
}
