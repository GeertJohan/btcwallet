/*
 * Copyright (c) 2013, 2014 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"fmt"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
	"github.com/conformal/go-flags"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultCAFilename     = "btcd.cert"
	defaultConfigFilename = "btcwallet.conf"
	defaultBtcNet         = btcwire.TestNet3
	defaultLogLevel       = "info"
	defaultKeypoolSize    = 100
	defaultDisallowFree   = false
)

var (
	btcdHomeDir        = btcutil.AppDataDir("btcd", false)
	btcwalletHomeDir   = btcutil.AppDataDir("btcwallet", false)
	defaultCAFile      = filepath.Join(btcwalletHomeDir, defaultCAFilename)
	btcdHomedirCAFile  = filepath.Join(btcdHomeDir, "rpc.cert")
	defaultConfigFile  = filepath.Join(btcwalletHomeDir, defaultConfigFilename)
	defaultDataDir     = btcwalletHomeDir
	defaultRPCKeyFile  = filepath.Join(btcwalletHomeDir, "rpc.key")
	defaultRPCCertFile = filepath.Join(btcwalletHomeDir, "rpc.cert")
)

type config struct {
	ShowVersion  bool     `short:"V" long:"version" description:"Display version information and exit"`
	CAFile       string   `long:"cafile" description:"File containing root certificates to authenticate a TLS connections with btcd"`
	RPCConnect   string   `short:"c" long:"rpcconnect" description:"Hostname/IP and port of btcd RPC server to connect to (default localhost:18334, mainnet: localhost:8334)"`
	DebugLevel   string   `short:"d" long:"debuglevel" description:"Logging level {trace, debug, info, warn, error, critical}"`
	ConfigFile   string   `short:"C" long:"configfile" description:"Path to configuration file"`
	SvrListeners []string `long:"rpclisten" description:"Listen for RPC/websocket connections on this interface/port (default port: 18332, mainnet: 8332)"`
	DataDir      string   `short:"D" long:"datadir" description:"Directory to store wallets and transactions"`
	Username     string   `short:"u" long:"username" description:"Username for btcd authorization"`
	Password     string   `short:"P" long:"password" default-mask:"-" description:"Password for btcd authorization"`
	RPCCert      string   `long:"rpccert" description:"File containing the certificate file"`
	RPCKey       string   `long:"rpckey" description:"File containing the certificate key"`
	MainNet      bool     `long:"mainnet" description:"Use the main Bitcoin network (default testnet3)"`
	KeypoolSize  uint     `short:"k" long:"keypoolsize" description:"Maximum number of addresses in keypool"`
	DisallowFree bool     `long:"disallowfree" description:"Force transactions to always include a fee"`
	Proxy        string   `long:"proxy" description:"Connect via SOCKS5 proxy (eg. 127.0.0.1:9050)"`
	ProxyUser    string   `long:"proxyuser" description:"Username for proxy server"`
	ProxyPass    string   `long:"proxypass" default-mask:"-" description:"Password for proxy server"`
	Profile      string   `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65536"`
}

// cleanAndExpandPath expands environement variables and leading ~ in the
// passed path, cleans the result, and returns it.
func cleanAndExpandPath(path string) string {
	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		homeDir := filepath.Dir(btcwalletHomeDir)
		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but they variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}

// removeDuplicateAddresses returns a new slice with all duplicate entries in
// addrs removed.
func removeDuplicateAddresses(addrs []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, val := range addrs {
		if _, ok := seen[val]; !ok {
			result = append(result, val)
			seen[val] = true
		}
	}
	return result
}

// normalizeAddresses returns a new slice with all the passed peer addresses
// normalized with the given default port, and all duplicates removed.
func normalizeAddresses(addrs []string, defaultPort string) []string {
	for i, addr := range addrs {
		addrs[i] = normalizeAddress(addr, defaultPort)
	}

	return removeDuplicateAddresses(addrs)
}

// filesExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// normalizeAddress returns addr with the passed default port appended if
// there is not already a port specified.
func normalizeAddress(addr, defaultPort string) string {
	_, _, err := net.SplitHostPort(addr)
	if err != nil {
		return net.JoinHostPort(addr, defaultPort)
	}
	return addr
}

// loadConfig initializes and parses the config using a config file and command
// line options.
//
// The configuration proceeds as follows:
//      1) Start with a default config with sane settings
//      2) Pre-parse the command line to check for an alternative config file
//      3) Load configuration file overwriting defaults with any specified options
//      4) Parse CLI options and overwrite/add any specified options
//
// The above results in btcwallet functioning properly without any config
// settings while still allowing the user to override settings with config files
// and command line options.  Command line options always take precedence.
func loadConfig() (*config, []string, error) {
	// Default config.
	cfg := config{
		DebugLevel:   defaultLogLevel,
		ConfigFile:   defaultConfigFile,
		DataDir:      defaultDataDir,
		RPCKey:       defaultRPCKeyFile,
		RPCCert:      defaultRPCCertFile,
		KeypoolSize:  defaultKeypoolSize,
		DisallowFree: defaultDisallowFree,
	}

	// A config file in the current directory takes precedence.
	if fileExists(defaultConfigFilename) {
		cfg.ConfigFile = defaultConfigFile
	}

	// Pre-parse the command line options to see if an alternative config
	// file or the version flag was specified.
	preCfg := cfg
	preParser := flags.NewParser(&preCfg, flags.Default)
	_, err := preParser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); !ok || e.Type != flags.ErrHelp {
			preParser.WriteHelp(os.Stderr)
		}
		return nil, nil, err
	}

	// Show the version and exit if the version flag was specified.
	if preCfg.ShowVersion {
		appName := filepath.Base(os.Args[0])
		appName = strings.TrimSuffix(appName, filepath.Ext(appName))
		fmt.Println(appName, "version", version())
		os.Exit(0)
	}

	// Load additional config from file.
	var configFileError error
	parser := flags.NewParser(&cfg, flags.Default)
	err = flags.NewIniParser(parser).ParseFile(preCfg.ConfigFile)
	if err != nil {
		if _, ok := err.(*os.PathError); !ok {
			fmt.Fprintln(os.Stderr, err)
			parser.WriteHelp(os.Stderr)
			return nil, nil, err
		}
		configFileError = err
	}

	// Parse command line options again to ensure they take precedence.
	remainingArgs, err := parser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); !ok || e.Type != flags.ErrHelp {
			parser.WriteHelp(os.Stderr)
		}
		return nil, nil, err
	}

	// Warn about missing config file after the final command line parse
	// succeeds.  This prevents the warning on help messages and invalid
	// options.
	if configFileError != nil {
		log.Warnf("%v", configFileError)
	}

	// Choose the active network params based on the mainnet net flag.
	if cfg.MainNet {
		activeNetParams = netParams(btcwire.MainNet)
	}

	// Validate debug log level
	if !validLogLevel(cfg.DebugLevel) {
		str := "%s: The specified debug level [%v] is invalid"
		err := fmt.Errorf(str, "loadConfig", cfg.DebugLevel)
		fmt.Fprintln(os.Stderr, err)
		parser.WriteHelp(os.Stderr)
		return nil, nil, err
	}

	if cfg.RPCConnect == "" {
		cfg.RPCConnect = activeNetParams.connect
	}

	// Add default port to connect flag if missing.
	cfg.RPCConnect = normalizeAddress(cfg.RPCConnect,
		activeNetParams.btcdPort)

	// If CAFile is unset, choose either the copy or local btcd cert.
	if cfg.CAFile == "" {
		cfg.CAFile = defaultCAFile

		// If the CA copy does not exist, check if we're connecting to
		// a local btcd and switch to its RPC cert if it exists.
		if !fileExists(cfg.CAFile) {
			host, _, err := net.SplitHostPort(cfg.RPCConnect)
			if err != nil {
				return nil, nil, err
			}
			switch host {
			case "localhost":
				fallthrough

			case "127.0.0.1":
				fallthrough

			case "::1":
				if fileExists(btcdHomedirCAFile) {
					cfg.CAFile = btcdHomedirCAFile
				}
			}
		}
	}

	if len(cfg.SvrListeners) == 0 {
		addrs, err := net.LookupHost("localhost")
		if err != nil {
			return nil, nil, err
		}
		cfg.SvrListeners = make([]string, 0, len(addrs))
		for _, addr := range addrs {
			addr = net.JoinHostPort(addr, activeNetParams.svrPort)
			cfg.SvrListeners = append(cfg.SvrListeners, addr)
		}
	}

	// Add default port to all rpc listener addresses if needed and remove
	// duplicate addresses.
	cfg.SvrListeners = normalizeAddresses(cfg.SvrListeners,
		activeNetParams.svrPort)

	// Expand environment variable and leading ~ for filepaths.
	cfg.CAFile = cleanAndExpandPath(cfg.CAFile)

	return &cfg, remainingArgs, nil
}

func (c *config) Net() btcwire.BitcoinNet {
	if cfg.MainNet {
		return btcwire.MainNet
	}
	return btcwire.TestNet3
}

// validLogLevel returns whether or not logLevel is a valid debug log level.
func validLogLevel(logLevel string) bool {
	switch logLevel {
	case "trace":
		fallthrough
	case "debug":
		fallthrough
	case "info":
		fallthrough
	case "warn":
		fallthrough
	case "error":
		fallthrough
	case "critical":
		return true
	}
	return false
}
