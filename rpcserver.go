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
	"bytes"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/conformal/btcec"
	"github.com/conformal/btcjson"
	"github.com/conformal/btcscript"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwallet/txstore"
	"github.com/conformal/btcwallet/wallet"
	"github.com/conformal/btcwire"
	"github.com/conformal/btcws"
	"sync"
	"time"
)

type cmdHandler func(btcjson.Cmd) (interface{}, *btcjson.Error)

var rpcHandlers = map[string]cmdHandler{
	// Standard bitcoind methods (implemented)
	"addmultisigaddress":     AddMultiSigAddress,
	"createmultisig":         CreateMultiSig,
	"dumpprivkey":            DumpPrivKey,
	"getaccount":             GetAccount,
	"getaccountaddress":      GetAccountAddress,
	"getaddressesbyaccount":  GetAddressesByAccount,
	"getbalance":             GetBalance,
	"getinfo":                GetInfo,
	"getnewaddress":          GetNewAddress,
	"getrawchangeaddress":    GetRawChangeAddress,
	"getreceivedbyaccount":   GetReceivedByAccount,
	"gettransaction":         GetTransaction,
	"importprivkey":          ImportPrivKey,
	"keypoolrefill":          KeypoolRefill,
	"listaccounts":           ListAccounts,
	"listsinceblock":         ListSinceBlock,
	"listtransactions":       ListTransactions,
	"listunspent":            ListUnspent,
	"sendfrom":               SendFrom,
	"sendmany":               SendMany,
	"sendtoaddress":          SendToAddress,
	"settxfee":               SetTxFee,
	"signmessage":            SignMessage,
	"signrawtransaction":     SignRawTransaction,
	"validateaddress":        ValidateAddress,
	"verifymessage":          VerifyMessage,
	"walletlock":             WalletLock,
	"walletpassphrase":       WalletPassphrase,
	"walletpassphrasechange": WalletPassphraseChange,

	// Standard bitcoind methods (currently unimplemented)
	"backupwallet":          Unimplemented,
	"dumpwallet":            Unimplemented,
	"getblocktemplate":      Unimplemented,
	"getreceivedbyaddress":  Unimplemented,
	"gettxout":              Unimplemented,
	"gettxoutsetinfo":       Unimplemented,
	"getwork":               Unimplemented,
	"importwallet":          Unimplemented,
	"listaddressgroupings":  Unimplemented,
	"listlockunspent":       Unimplemented,
	"listreceivedbyaccount": Unimplemented,
	"listreceivedbyaddress": Unimplemented,
	"lockunspent":           Unimplemented,
	"move":                  Unimplemented,
	"setaccount":            Unimplemented,
	"stop":                  Unimplemented,

	// Standard bitcoind methods which won't be implemented by btcwallet.
	"encryptwallet": Unsupported,

	// Extensions not exclusive to websocket connections.
	"createencryptedwallet": CreateEncryptedWallet,
}

// Extensions exclusive to websocket connections.
var wsHandlers = map[string]cmdHandler{
	"exportwatchingwallet":    ExportWatchingWallet,
	"getaddressbalance":       GetAddressBalance,
	"getunconfirmedbalance":   GetUnconfirmedBalance,
	"listaddresstransactions": ListAddressTransactions,
	"listalltransactions":     ListAllTransactions,
	"recoveraddresses":        RecoverAddresses,
	"walletislocked":          WalletIsLocked,
}

// Channels to control RPCGateway
var (
	// Incoming requests from frontends
	clientRequests = make(chan *ClientRequest)

	// Incoming notifications from a bitcoin server (btcd)
	svrNtfns = make(chan btcjson.Cmd)
)

// ErrServerBusy is a custom JSON-RPC error for when a client's request
// could not be added to the server request queue for handling.
var ErrServerBusy = btcjson.Error{
	Code:    -32000,
	Message: "Server busy",
}

// ErrServerBusyRaw is the raw JSON encoding of ErrServerBusy.
var ErrServerBusyRaw = json.RawMessage(`{"code":-32000,"message":"Server busy"}`)

// RPCGateway is the common entry point for all client RPC requests and
// server notifications.  If a request needs to be handled by btcwallet,
// it is sent to WalletRequestProcessor's request queue, or dropped if the
// queue is full.  If a request is unhandled, it is recreated with a new
// JSON-RPC id and sent to btcd for handling.  Notifications are also queued
// if they cannot be immediately handled, but are never dropped (queue may
// grow infinitely large).
func RPCGateway() {
	var ntfnQueue []btcjson.Cmd
	unreadChan := make(chan btcjson.Cmd)

	for {
		var ntfnOut chan btcjson.Cmd
		var oldestNtfn btcjson.Cmd
		if len(ntfnQueue) > 0 {
			ntfnOut = handleNtfn
			oldestNtfn = ntfnQueue[0]
		} else {
			ntfnOut = unreadChan
		}

		select {
		case r := <-clientRequests:
			// Check whether to handle request or send to btcd.
			_, std := rpcHandlers[r.request.Method()]
			_, ext := wsHandlers[r.request.Method()]
			if std || ext {
				select {
				case requestQueue <- r:
				default:
					// Server busy with too many requests.
					resp := RawRPCResponse{
						Error: &ErrServerBusyRaw,
					}
					r.response <- resp
				}
			} else {
				r.request.SetId(<-NewJSONID)
				request := &ServerRequest{
					request:  r.request,
					response: r.response,
				}
				CurrentServerConn().SendRequest(request)
			}

		case n := <-svrNtfns:
			ntfnQueue = append(ntfnQueue, n)

		case ntfnOut <- oldestNtfn:
			ntfnQueue = ntfnQueue[1:]
		}
	}
}

// Channels to control WalletRequestProcessor
var (
	requestQueue = make(chan *ClientRequest, 100)
	handleNtfn   = make(chan btcjson.Cmd)
)

// WalletRequestProcessor processes client requests and btcd notifications.
func WalletRequestProcessor() {
	for {
		select {
		case r := <-requestQueue:
			method := r.request.Method()
			f, ok := rpcHandlers[method]
			if !ok && r.ws {
				f, ok = wsHandlers[method]
			}
			if !ok {
				f = Unimplemented
			}

			AcctMgr.Grab()
			result, jsonErr := f(r.request)
			AcctMgr.Release()

			if jsonErr != nil {
				b, _ := json.Marshal(jsonErr)
				r.response <- RawRPCResponse{
					Error: (*json.RawMessage)(&b),
				}
			} else {
				b, _ := json.Marshal(result)
				r.response <- RawRPCResponse{
					Result: (*json.RawMessage)(&b),
				}
			}

		case n := <-handleNtfn:
			f, ok := notificationHandlers[n.Method()]
			if !ok {
				// Ignore unhandled notifications.
				continue
			}

			AcctMgr.Grab()
			err := f(n)
			AcctMgr.Release()
			switch err {
			case txstore.ErrInconsistentStore:
				// Assume this is a broken btcd reordered
				// notifications.  Restart the connection
				// to reload accounts files from their last
				// known good state.
				log.Warn("Reconnecting to recover from " +
					"out-of-order btcd notification")
				s := CurrentServerConn()
				if btcd, ok := s.(*BtcdRPCConn); ok {
					AcctMgr.Grab()
					btcd.Close()
					AcctMgr.OpenAccounts()
					AcctMgr.Release()
				}

			case nil: // ignore
			default:
				log.Warn(err)
			}
		}
	}
}

// Unimplemented handles an unimplemented RPC request with the
// appropiate error.
func Unimplemented(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	return nil, &btcjson.ErrUnimplemented
}

// Unsupported handles a standard bitcoind RPC request which is
// unsupported by btcwallet due to design differences.
func Unsupported(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	e := btcjson.Error{
		Code:    -1,
		Message: "Request unsupported by btcwallet",
	}
	return nil, &e
}

// makeMultiSigScript is a heper function to combine common logic for
// AddMultiSig and CreateMultiSig.
// all error codes are rpc parse error here to match bitcoind which just throws
// a runtime exception. *sigh*.
func makeMultiSigScript(keys []string, nRequired int) ([]byte, *btcjson.Error) {
	keysesPrecious := make([]*btcutil.AddressPubKey, len(keys))

	// The address list will made up either of addreseses (pubkey hash), for
	// which we need to look up the keys in wallet, straight pubkeys, or a
	// mixture of the two.
	for i, a := range keys {
		// try to parse as pubkey address
		a, err := btcutil.DecodeAddress(a, cfg.Net())
		if err != nil {
			return nil, &btcjson.Error{
				Code:    btcjson.ErrParse.Code,
				Message: err.Error(),
			}
		}

		switch addr := a.(type) {
		case *btcutil.AddressPubKey:
			keysesPrecious[i] = addr
		case *btcutil.AddressPubKeyHash:
			ainfo, err := AcctMgr.Address(addr)
			if err != nil {
				return nil, &btcjson.Error{
					Code:    btcjson.ErrParse.Code,
					Message: err.Error(),
				}
			}

			apkinfo := ainfo.(wallet.PubKeyAddress)

			// This will be an addresspubkey
			a, err := btcutil.DecodeAddress(apkinfo.ExportPubKey(),
				cfg.Net())
			if err != nil {
				return nil, &btcjson.Error{
					Code:    btcjson.ErrParse.Code,
					Message: err.Error(),
				}
			}

			apk := a.(*btcutil.AddressPubKey)
			keysesPrecious[i] = apk
		default:
			return nil, &btcjson.Error{
				Code:    btcjson.ErrParse.Code,
				Message: "key is not a pubkey or pubkey hash address",
			}
		}
	}

	script, err := btcscript.MultiSigScript(keysesPrecious, nRequired)
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrParse.Code,
			Message: err.Error(),
		}
	}

	return script, nil
}

// AddMultiSigAddress handles an addmultisigaddress request by adding a
// multisig address to the given wallet.
func AddMultiSigAddress(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.AddMultisigAddressCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	acct, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default:
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	script, jsonerr := makeMultiSigScript(cmd.Keys, cmd.NRequired)
	if jsonerr != nil {
		return nil, jsonerr
	}

	// TODO(oga) blockstamp current block?
	address, err := acct.ImportScript(script, &wallet.BlockStamp{})
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	return address.EncodeAddress(), nil
}

// CreateMultiSig handles an createmultisig request by returning a
// multisig address for the given inputs.
func CreateMultiSig(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.CreateMultisigCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	script, jsonerr := makeMultiSigScript(cmd.Keys, cmd.NRequired)
	if jsonerr != nil {
		return nil, jsonerr
	}

	address, err := btcutil.NewAddressScriptHash(script, cfg.Net())
	if err != nil {
		// above is a valid script, shouldn't happen.
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	return btcjson.CreateMultiSigResult{
		Address:      address.EncodeAddress(),
		RedeemScript: hex.EncodeToString(script),
	}, nil
}

// DumpPrivKey handles a dumpprivkey request with the private key
// for a single address, or an appropiate error if the wallet
// is locked.
func DumpPrivKey(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.DumpPrivKeyCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	addr, err := btcutil.DecodeAddress(cmd.Address, cfg.Net())
	if err != nil {
		return nil, &btcjson.ErrInvalidAddressOrKey
	}

	switch key, err := AcctMgr.DumpWIFPrivateKey(addr); err {
	case nil:
		// Key was found.
		return key, nil

	case wallet.ErrWalletLocked:
		// Address was found, but the private key isn't
		// accessible.
		return nil, &btcjson.ErrWalletUnlockNeeded

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// DumpWallet handles a dumpwallet request by returning  all private
// keys in a wallet, or an appropiate error if the wallet is locked.
// TODO: finish this to match bitcoind by writing the dump to a file.
func DumpWallet(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	_, ok := icmd.(*btcjson.DumpWalletCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	switch keys, err := AcctMgr.DumpKeys(); err {
	case nil:
		// Reply with sorted WIF encoded private keys
		return keys, nil

	case wallet.ErrWalletLocked:
		return nil, &btcjson.ErrWalletUnlockNeeded

	default: // any other non-nil error
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// ExportWatchingWallet handles an exportwatchingwallet request by exporting
// the current account wallet as a watching wallet (with no private keys), and
// either writing the exported wallet to disk, or base64-encoding serialized
// account files and sending them back in the response.
func ExportWatchingWallet(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.ExportWatchingWalletCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	wa, err := a.ExportWatchingWallet()
	if err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	if cmd.Download {
		switch m, err := wa.exportBase64(); err {
		case nil:
			return m, nil

		default:
			e := btcjson.Error{
				Code:    btcjson.ErrWallet.Code,
				Message: err.Error(),
			}
			return nil, &e
		}
	}

	// Create export directory, write files there.
	if err = wa.ExportToDirectory("watchingwallet"); err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	return nil, nil
}

// GetAddressesByAccount handles a getaddressesbyaccount request by returning
// all addresses for an account, or an error if the requested account does
// not exist.
func GetAddressesByAccount(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetAddressesByAccountCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	switch a, err := AcctMgr.Account(cmd.Account); err {
	case nil:
		// Return sorted active payment addresses.
		return a.SortedActivePaymentAddresses(), nil

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// GetBalance handles a getbalance request by returning the balance for an
// account (wallet), or an error if the requested account does not
// exist.
func GetBalance(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetBalanceCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	balance, err := AcctMgr.CalculateBalance(cmd.Account, cmd.MinConf)
	if err != nil {
		return nil, &btcjson.ErrWalletInvalidAccountName
	}

	// Return calculated balance.
	return balance, nil
}

// GetInfo handles a getinfo request by returning the a structure containing
// information about the current state of btcwallet.
// exist.
func GetInfo(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Call down to btcd for all of the information in this command known
	// by them.  This call can not realistically ever fail.
	gicmd, _ := btcjson.NewGetInfoCmd(<-NewJSONID)
	response := <-CurrentServerConn().SendRequest(NewServerRequest(gicmd))

	var info btcjson.InfoResult
	_, jsonErr := response.FinishUnmarshal(&info)
	if jsonErr != nil {
		return nil, jsonErr
	}

	balance := float64(0.0)
	accounts := AcctMgr.ListAccounts(1)
	for _, v := range accounts {
		balance += v
	}
	info.WalletVersion = int(wallet.VersCurrent.Uint32())
	info.Balance = balance
	// Keypool times are not tracked. set to current time.
	info.KeypoolOldest = time.Now().Unix()
	info.KeypoolSize = int(cfg.KeypoolSize)
	TxFeeIncrement.Lock()
	info.PaytxFee = float64(TxFeeIncrement.i) / float64(btcutil.SatoshiPerBitcoin)
	TxFeeIncrement.Unlock()
	/*
	 * We don't set the following since they don't make much sense in the
	 * wallet architecture:
	 *  - unlocked_until
	 *  - errors
	 */

	return info, nil
}

// GetAccount handles a getaccount request by returning the account name
// associated with a single address.
func GetAccount(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetAccountCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Is address valid?
	addr, err := btcutil.DecodeAddress(cmd.Address, cfg.Net())
	if err != nil {
		return nil, &btcjson.ErrInvalidAddressOrKey
	}
	if !addr.IsForNet(cfg.Net()) {
		return nil, &btcjson.ErrInvalidAddressOrKey
	}

	// Look up account which holds this address.
	acct, err := AcctMgr.AccountByAddress(addr)
	if err == ErrNotFound {
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidAddressOrKey.Code,
			Message: "Address not found in wallet",
		}
		return nil, &e
	}

	return acct.Name(), nil
}

// GetAccountAddress handles a getaccountaddress by returning the most
// recently-created chained address that has not yet been used (does not yet
// appear in the blockchain, or any tx that has arrived in the btcd mempool).
// If the most recently-requested address has been used, a new address (the
// next chained address in the keypool) is used.  This can fail if the keypool
// runs out (and will return btcjson.ErrWalletKeypoolRanOut if that happens).
func GetAccountAddress(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetAccountAddressCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Lookup account for this request.
	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	switch addr, err := a.CurrentAddress(); err {
	case nil:
		return addr.EncodeAddress(), nil

	case wallet.ErrWalletLocked:
		return nil, &btcjson.ErrWalletKeypoolRanOut

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// GetAddressBalance handles a getaddressbalance extension request by
// returning the current balance (sum of unspent transaction output amounts)
// for a single address.
func GetAddressBalance(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.GetAddressBalanceCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Is address valid?
	addr, err := btcutil.DecodeAddress(cmd.Address, cfg.Net())
	if err != nil {
		return nil, &btcjson.ErrInvalidAddressOrKey
	}

	// Get the account which holds the address in the request.
	// This should not fail, so if it does, return an internal
	// error to the frontend.
	a, err := AcctMgr.AccountByAddress(addr)
	if err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidAddressOrKey.Code,
			Message: "Address not found in wallet",
		}
		return nil, &e
	}

	return a.CalculateAddressBalance(addr, int(cmd.Minconf)), nil
}

// GetUnconfirmedBalance handles a getunconfirmedbalance extension request
// by returning the current unconfirmed balance of an account.
func GetUnconfirmedBalance(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.GetUnconfirmedBalanceCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Get the account included in the request.
	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default:
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	confirmed := a.CalculateBalance(1)
	unconfirmed := a.CalculateBalance(0) - confirmed
	return unconfirmed, nil
}

// ImportPrivKey handles an importprivkey request by parsing
// a WIF-encoded private key and adding it to an account.
func ImportPrivKey(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.ImportPrivKeyCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Get the acount included in the request. Yes, Label is the
	// account name...
	a, err := AcctMgr.Account(cmd.Label)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default:
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	pk, net, compressed, err := btcutil.DecodePrivateKey(cmd.PrivKey)
	if err != nil || net != a.Net() {
		return nil, &btcjson.ErrInvalidAddressOrKey
	}

	// Import the private key, handling any errors.
	bs := &wallet.BlockStamp{}
	switch _, err := a.ImportPrivateKey(pk, compressed, bs, cmd.Rescan); err {
	case nil:
		// If the import was successful, reply with nil.
		return nil, nil

	case wallet.ErrDuplicate:
		// Do not return duplicate key errors to the client.
		return nil, nil

	case wallet.ErrWalletLocked:
		return nil, &btcjson.ErrWalletUnlockNeeded

	default:
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// KeypoolRefill handles the keypoolrefill command. Since we handle the keypool
// automatically this does nothing since refilling is never manually required.
func KeypoolRefill(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	return nil, nil
}

// NotifyBalances notifies an attached frontend of the current confirmed
// and unconfirmed account balances.
//
// TODO(jrick): Switch this to return a single JSON object
// (map[string]interface{}) of all accounts and their balances, instead of
// separate notifications for each account.
func NotifyBalances(frontend chan []byte) {
	AcctMgr.NotifyBalances(frontend)
}

// GetNewAddress handlesa getnewaddress request by returning a new
// address for an account.  If the account does not exist or the keypool
// ran out with a locked wallet, an appropiate error is returned.
func GetNewAddress(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetNewAddressCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	case ErrBtcdDisconnected:
		return nil, &ErrBtcdDisconnected

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	addr, err := a.NewAddress()
	if err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	// Return the new payment address string.
	return addr.EncodeAddress(), nil
}

// GetRawChangeAddress handles a getrawchangeaddress request by creating
// and returning a new change address for an account.
//
// Note: bitcoind allows specifying the account as an optional parameter,
// but ignores the parameter.
func GetRawChangeAddress(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.GetRawChangeAddressCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	addr, err := a.NewChangeAddress()
	if err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	// Return the new payment address string.
	return addr.EncodeAddress(), nil
}

// GetReceivedByAccount handles a getreceivedbyaccount request by returning
// the total amount received by addresses of an account.
func GetReceivedByAccount(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.GetReceivedByAccountCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	amt, err := a.TotalReceived(cmd.MinConf)
	if err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	return amt, nil
}

func GetTransaction(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.GetTransactionCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	txsha, err := btcwire.NewShaHashFromStr(cmd.Txid)
	if err != nil {
		return nil, &btcjson.ErrDecodeHexString
	}

	accumulatedTxen := AcctMgr.GetTransaction(txsha)
	if len(accumulatedTxen) == 0 {
		return nil, &btcjson.ErrNoTxInfo
	}

	received := btcutil.Amount(0)
	var debitTx *txstore.TxRecord
	var debitAccount string

	ret := btcjson.GetTransactionResult{
		Details:         []btcjson.GetTransactionDetailsResult{},
		WalletConflicts: []string{},
	}
	details := []btcjson.GetTransactionDetailsResult{}
	for _, e := range accumulatedTxen {
		for _, cred := range e.Tx.Credits() {
			// Change is ignored.
			if cred.Change() {
				continue
			}

			received += cred.Amount()

			var addr string
			_, addrs, _, _ := cred.Addresses(cfg.Net())
			if len(addrs) == 1 {
				addr = addrs[0].EncodeAddress()
			}

			details = append(details, btcjson.GetTransactionDetailsResult{
				Account: e.Account,
				// TODO(oga) We don't mine for now so there
				// won't be any special coinbase types. If the
				// tx is a coinbase then we should handle it
				// specially with the category depending on
				// whether it is an orphan or in the blockchain.
				Category: "receive",
				Amount:   cred.Amount().ToUnit(btcutil.AmountBTC),
				Address:  addr,
			})
		}

		if e.Tx.Debits() != nil {
			// There should only be a single debits record for any
			// of the account's transaction records.
			debitTx = e.Tx
			debitAccount = e.Account
		}
	}

	totalAmount := received
	if debitTx != nil {
		debits := debitTx.Debits()
		totalAmount -= debits.InputAmount()
		info := btcjson.GetTransactionDetailsResult{
			Account:  debitAccount,
			Category: "send",
			// negative since it is a send
			Amount: (-debits.OutputAmount(true)).ToUnit(btcutil.AmountBTC),
			Fee:    debits.Fee().ToUnit(btcutil.AmountBTC),
		}
		_, addrs, _, _ := debitTx.Credits()[0].Addresses(cfg.Net())
		if len(addrs) == 1 {
			info.Address = addrs[0].EncodeAddress()
		}
		ret.Fee += info.Fee
		// Add sent information to front.
		ret.Details = append(ret.Details, info)

	}
	ret.Details = append(ret.Details, details...)

	ret.Amount = totalAmount.ToUnit(btcutil.AmountBTC)

	// Generic information should be the same, so just use the first one.
	first := accumulatedTxen[0]
	ret.TxID = first.Tx.Tx().Sha().String()

	buf := bytes.NewBuffer(nil)
	buf.Grow(first.Tx.Tx().MsgTx().SerializeSize())
	err = first.Tx.Tx().MsgTx().Serialize(buf)
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}
	ret.Hex = hex.EncodeToString(buf.Bytes())

	// TODO(oga) technically we have different time and
	// timereceived depending on if a transaction was send or
	// receive. We ideally should provide the correct numbers for
	// both. Right now they will always be the same
	ret.Time = first.Tx.Received().Unix()
	ret.TimeReceived = first.Tx.Received().Unix()
	if txr := first.Tx; txr.BlockHeight != -1 {
		txBlock, err := txr.Block()
		if err != nil {
			return nil, &btcjson.Error{
				Code:    btcjson.ErrWallet.Code,
				Message: err.Error(),
			}
		}
		bs, err := GetCurBlock()
		if err != nil {
			return nil, &btcjson.Error{
				Code:    btcjson.ErrWallet.Code,
				Message: err.Error(),
			}
		}
		ret.BlockIndex = int64(first.Tx.Tx().Index())
		ret.BlockHash = txBlock.Hash.String()
		ret.BlockTime = txBlock.Time.Unix()
		ret.Confirmations = int64(txr.Confirmations(bs.Height))
	}
	// TODO(oga) if the tx is a coinbase we should set "generated" to true.
	// Since we do not mine this currently is never the case.
	return ret, nil
}

// ListAccounts handles a listaccounts request by returning a map of account
// names to their balances.
func ListAccounts(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.ListAccountsCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Return the map.  This will be marshaled into a JSON object.
	return AcctMgr.ListAccounts(cmd.MinConf), nil
}

// ListSinceBlock handles a listsinceblock request by returning an array of maps
// with details of sent and received wallet transactions since the given block.
func ListSinceBlock(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.ListSinceBlockCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	height := int32(-1)
	if cmd.BlockHash != "" {
		br, err := GetBlock(CurrentServerConn(), cmd.BlockHash)
		if err != nil {
			return nil, err
		}
		height = int32(br.Height)
	}

	bs, err := GetCurBlock()
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	// For the result we need the block hash for the last block counted
	// in the blockchain due to confirmations. We send this off now so that
	// it can arrive asynchronously while we figure out the rest.
	gbh, err := btcjson.NewGetBlockHashCmd(<-NewJSONID,
		int64(bs.Height)+1-int64(cmd.TargetConfirmations))
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	bhChan := CurrentServerConn().SendRequest(NewServerRequest(gbh))

	txInfoList, err := AcctMgr.ListSinceBlock(height, bs.Height,
		cmd.TargetConfirmations)
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	// Done with work, get the response.
	response := <-bhChan
	var hash string
	_, jsonErr := response.FinishUnmarshal(&hash)
	if jsonErr != nil {
		return nil, jsonErr
	}

	res := btcjson.ListSinceBlockResult{
		Transactions: txInfoList,
		LastBlock:    hash,
	}

	return res, nil
}

// ListTransactions handles a listtransactions request by returning an
// array of maps with details of sent and recevied wallet transactions.
func ListTransactions(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.ListTransactionsCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	switch txList, err := a.ListTransactions(cmd.From, cmd.Count); err {
	case nil:
		// Return the list of tx information.
		return txList, nil

	case ErrBtcdDisconnected:
		e := btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: "btcd disconnected",
		}
		return nil, &e

	default:
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// ListAddressTransactions handles a listaddresstransactions request by
// returning an array of maps with details of spent and received wallet
// transactions.  The form of the reply is identical to listtransactions,
// but the array elements are limited to transaction details which are
// about the addresess included in the request.
func ListAddressTransactions(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.ListAddressTransactionsCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	// Decode addresses.
	pkHashMap := make(map[string]struct{})
	for _, addrStr := range cmd.Addresses {
		addr, err := btcutil.DecodeAddress(addrStr, cfg.Net())
		if err != nil {
			return nil, &btcjson.ErrInvalidAddressOrKey
		}
		apkh, ok := addr.(*btcutil.AddressPubKeyHash)
		if !ok || !apkh.IsForNet(cfg.Net()) {
			return nil, &btcjson.ErrInvalidAddressOrKey
		}
		pkHashMap[string(addr.ScriptAddress())] = struct{}{}
	}

	txList, err := a.ListAddressTransactions(pkHashMap)
	if err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
	return txList, nil
}

// ListAllTransactions handles a listalltransactions request by returning
// a map with details of sent and recevied wallet transactions.  This is
// similar to ListTransactions, except it takes only a single optional
// argument for the account name and replies with all transactions.
func ListAllTransactions(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.ListAllTransactionsCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	switch txList, err := a.ListAllTransactions(); err {
	case nil:
		// Return the list of tx information.
		return txList, nil

	case ErrBtcdDisconnected:
		e := btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: "btcd disconnected",
		}
		return nil, &e

	default:
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// ListUnspent handles the listunspent command.
func ListUnspent(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.ListUnspentCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}
	addresses := make(map[string]bool)
	if len(cmd.Addresses) != 0 {
		// confirm that all of them are good:
		for _, as := range cmd.Addresses {
			a, err := btcutil.DecodeAddress(as, cfg.Net())
			if err != nil {
				return nil, &btcjson.ErrInvalidAddressOrKey
			}

			if _, ok := addresses[a.EncodeAddress()]; ok {
				// duplicate
				return nil, &btcjson.ErrInvalidParameter
			}
			addresses[a.EncodeAddress()] = true
		}
	}

	results, err := AcctMgr.ListUnspent(cmd.MinConf, cmd.MaxConf, addresses)
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	return results, nil
}

// sendPairs is a helper routine to reduce duplicated code when creating and
// sending payment transactions.
func sendPairs(icmd btcjson.Cmd, account string, amounts map[string]btcutil.Amount,
	minconf int) (interface{}, *btcjson.Error) {
	// Check that the account specified in the request exists.
	a, err := AcctMgr.Account(account)
	if err != nil {
		return nil, &btcjson.ErrWalletInvalidAccountName
	}

	// Create transaction, replying with an error if the creation
	// was not successful.
	createdTx, err := a.txToPairs(amounts, minconf)
	switch {
	case err == ErrNonPositiveAmount:
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "amount must be positive",
		}
		return nil, &e

	case err == wallet.ErrWalletLocked:
		return nil, &btcjson.ErrWalletUnlockNeeded

	case err != nil: // any other non-nil error
		e := btcjson.Error{
			Code:    btcjson.ErrInternal.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	// Mark txid as having send history so handlers adding receive history
	// wait until all send history has been written.
	SendTxHistSyncChans.add <- *createdTx.tx.Sha()

	// If a change address was added, sync wallet to disk and request
	// transaction notifications to the change address.
	if createdTx.changeAddr != nil {
		AcctMgr.ds.ScheduleWalletWrite(a)
		if err := AcctMgr.ds.FlushAccount(a); err != nil {
			e := btcjson.Error{
				Code:    btcjson.ErrWallet.Code,
				Message: "Cannot write account: " + err.Error(),
			}
			return nil, &e
		}
		a.ReqNewTxsForAddress(createdTx.changeAddr)
	}

	serializedTx := bytes.NewBuffer(nil)
	serializedTx.Grow(createdTx.tx.MsgTx().SerializeSize())
	createdTx.tx.MsgTx().Serialize(serializedTx)
	hextx := hex.EncodeToString(serializedTx.Bytes())
	txSha, jsonErr := SendRawTransaction(CurrentServerConn(), hextx)
	if jsonErr != nil {
		SendTxHistSyncChans.remove <- *createdTx.tx.Sha()
		return nil, jsonErr
	}

	return handleSendRawTxReply(icmd, txSha, a, createdTx)
}

// SendFrom handles a sendfrom RPC request by creating a new transaction
// spending unspent transaction outputs for a wallet to another payment
// address.  Leftover inputs not sent to the payment address or a fee for
// the miner are sent back to a new address in the wallet.  Upon success,
// the TxID for the created transaction is returned.
func SendFrom(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SendFromCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Check that signed integer parameters are positive.
	if cmd.Amount < 0 {
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "amount must be positive",
		}
		return nil, &e
	}
	if cmd.MinConf < 0 {
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "minconf must be positive",
		}
		return nil, &e
	}
	// Create map of address and amount pairs.
	pairs := map[string]btcutil.Amount{
		cmd.ToAddress: btcutil.Amount(cmd.Amount),
	}

	return sendPairs(cmd, cmd.FromAccount, pairs, cmd.MinConf)
}

// SendMany handles a sendmany RPC request by creating a new transaction
// spending unspent transaction outputs for a wallet to any number of
// payment addresses.  Leftover inputs not sent to the payment address
// or a fee for the miner are sent back to a new address in the wallet.
// Upon success, the TxID for the created transaction is returned.
func SendMany(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SendManyCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Check that minconf is positive.
	if cmd.MinConf < 0 {
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "minconf must be positive",
		}
		return nil, &e
	}

	// Recreate address/amount pairs, using btcutil.Amount.
	pairs := make(map[string]btcutil.Amount, len(cmd.Amounts))
	for k, v := range cmd.Amounts {
		pairs[k] = btcutil.Amount(v)
	}

	return sendPairs(cmd, cmd.FromAccount, pairs, cmd.MinConf)
}

// SendToAddress handles a sendtoaddress RPC request by creating a new
// transaction spending unspent transaction outputs for a wallet to another
// payment address.  Leftover inputs not sent to the payment address or a fee
// for the miner are sent back to a new address in the wallet.  Upon success,
// the TxID for the created transaction is returned.
func SendToAddress(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SendToAddressCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Check that signed integer parameters are positive.
	if cmd.Amount < 0 {
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidParameter.Code,
			Message: "amount must be positive",
		}
		return nil, &e
	}

	// Mock up map of address and amount pairs.
	pairs := map[string]btcutil.Amount{
		cmd.Address: btcutil.Amount(cmd.Amount),
	}

	return sendPairs(cmd, "", pairs, 1)
}

// Channels to manage SendBeforeReceiveHistorySync.
var SendTxHistSyncChans = struct {
	add, done, remove chan btcwire.ShaHash
	access            chan SendTxHistSyncRequest
}{
	add:    make(chan btcwire.ShaHash),
	remove: make(chan btcwire.ShaHash),
	done:   make(chan btcwire.ShaHash),
	access: make(chan SendTxHistSyncRequest),
}

// SendTxHistSyncRequest requests a SendTxHistSyncResponse from
// SendBeforeReceiveHistorySync.
type SendTxHistSyncRequest struct {
	txsha    btcwire.ShaHash
	response chan SendTxHistSyncResponse
}

// SendTxHistSyncResponse is the response
type SendTxHistSyncResponse struct {
	c  chan struct{}
	ok bool
}

// SendBeforeReceiveHistorySync manages a set of transaction hashes
// created by this wallet.  For each newly added txsha, a channel is
// created.  Once the send history has been recorded, the txsha should
// be messaged across done, causing the internal channel to be closed.
// Before receive history is recorded, access should be used to check
// if there are or were any goroutines writing send history, and if
// so, wait until the channel is closed after a done message.
func SendBeforeReceiveHistorySync(add, done, remove chan btcwire.ShaHash,
	access chan SendTxHistSyncRequest) {

	m := make(map[btcwire.ShaHash]chan struct{})
	for {
		select {
		case txsha := <-add:
			m[txsha] = make(chan struct{})

		case txsha := <-remove:
			delete(m, txsha)

		case txsha := <-done:
			if c, ok := m[txsha]; ok {
				close(c)
			}

		case req := <-access:
			c, ok := m[req.txsha]
			req.response <- SendTxHistSyncResponse{c: c, ok: ok}
		}
	}
}

func handleSendRawTxReply(icmd btcjson.Cmd, txIDStr string, a *Account, txInfo *CreatedTx) (interface{}, *btcjson.Error) {
	// Add to transaction store.
	txr, err := a.TxStore.InsertTx(txInfo.tx, nil)
	if err != nil {
		log.Warnf("Error adding sent tx history: %v", err)
		return nil, &btcjson.ErrInternal
	}
	debits, err := txr.AddDebits(txInfo.inputs)
	if err != nil {
		log.Warnf("Error adding sent tx history: %v", err)
		return nil, &btcjson.ErrInternal
	}
	AcctMgr.ds.ScheduleTxStoreWrite(a)

	// Notify frontends of new SendTx.
	bs, err := GetCurBlock()
	if err == nil {
		ltr, err := debits.ToJSON(a.Name(), bs.Height, a.Net())
		if err != nil {
			log.Warnf("Error adding sent tx history: %v", err)
			return nil, &btcjson.ErrInternal
		}
		for _, details := range ltr {
			NotifyNewTxDetails(allClients, a.Name(), details)
		}
	}

	// Signal that received notifiations are ok to add now.
	SendTxHistSyncChans.done <- *txInfo.tx.Sha()

	// Disk sync tx and utxo stores.
	if err := AcctMgr.ds.FlushAccount(a); err != nil {
		log.Errorf("cannot write account: %v", err)
	}

	// Notify all frontends of account's new unconfirmed and
	// confirmed balance.
	confirmed := a.CalculateBalance(1)
	unconfirmed := a.CalculateBalance(0) - confirmed
	NotifyWalletBalance(allClients, a.name, confirmed)
	NotifyWalletBalanceUnconfirmed(allClients, a.name, unconfirmed)

	// The comments to be saved differ based on the underlying type
	// of the cmd, so switch on the type to check whether it is a
	// SendFromCmd or SendManyCmd.
	//
	// TODO(jrick): If message succeeded in being sent, save the
	// transaction details with comments.
	switch cmd := icmd.(type) {
	case *btcjson.SendFromCmd:
		_ = cmd.Comment
		_ = cmd.CommentTo

	case *btcjson.SendManyCmd:
		_ = cmd.Comment
	case *btcjson.SendToAddressCmd:
		_ = cmd.Comment
		_ = cmd.CommentTo
	}

	log.Infof("Successfully sent transaction %v", txIDStr)
	return txIDStr, nil
}

// SetTxFee sets the transaction fee per kilobyte added to transactions.
func SetTxFee(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SetTxFeeCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	// Check that amount is not negative.
	if cmd.Amount < 0 {
		e := btcjson.Error{
			Code:    btcjson.ErrInvalidParams.Code,
			Message: "amount cannot be negative",
		}
		return nil, &e
	}

	// Set global tx fee.
	TxFeeIncrement.Lock()
	TxFeeIncrement.i = btcutil.Amount(cmd.Amount)
	TxFeeIncrement.Unlock()

	// A boolean true result is returned upon success.
	return true, nil
}

// SignMessage signs the given message with the private key for the given
// address
func SignMessage(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.SignMessageCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	addr, err := btcutil.DecodeAddress(cmd.Address, cfg.Net())
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrParse.Code,
			Message: err.Error(),
		}
	}

	ainfo, err := AcctMgr.Address(addr)
	if err != nil {
		return nil, &btcjson.ErrInvalidAddressOrKey
	}

	pka := ainfo.(wallet.PubKeyAddress)
	privkey, err := pka.PrivKey()
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	fullmsg := "Bitcoin Signed Message:\n" + cmd.Message
	sigbytes, err := btcec.SignCompact(btcec.S256(), privkey,
		btcwire.DoubleSha256([]byte(fullmsg)), ainfo.Compressed())
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	return base64.StdEncoding.EncodeToString(sigbytes), nil
}

// CreateEncryptedWallet creates a new account with an encrypted
// wallet.  If an account with the same name as the requested account
// name already exists, an invalid account name error is returned to
// the client.
//
// Wallets will be created on TestNet3, or MainNet if btcwallet is run with
// the --mainnet option.
func CreateEncryptedWallet(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.CreateEncryptedWalletCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	err := AcctMgr.CreateEncryptedWallet([]byte(cmd.Passphrase))
	switch err {
	case nil:
		// A nil reply is sent upon successful wallet creation.
		return nil, nil

	case ErrWalletExists:
		return nil, &btcjson.ErrWalletInvalidAccountName

	case ErrBtcdDisconnected:
		return nil, &ErrBtcdDisconnected

	default: // all other non-nil errors
		return nil, &btcjson.ErrInternal
	}
}

// RecoverAddresses recovers the next n addresses from an account's wallet.
func RecoverAddresses(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcws.RecoverAddressesCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	if err := a.RecoverAddresses(cmd.N); err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	return nil, nil
}

// pendingTx is used for async fetching of transaction dependancies in
// SignRawTransaction.
type pendingTx struct {
	resp   chan RawRPCResponse
	inputs []uint32 // list of inputs that care about this tx.
}

// keyInfo is used to store provided keys in SignRawTransaction.
type keyInfo struct {
	key        *ecdsa.PrivateKey
	compressed bool
}

// SignRawTransaction handles the signrawtransaction command.
func SignRawTransaction(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.SignRawTransactionCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	serializedTx, err := hex.DecodeString(cmd.RawTx)
	if err != nil {
		return nil, &btcjson.ErrDecodeHexString
	}
	msgTx := btcwire.NewMsgTx()
	err = msgTx.Deserialize(bytes.NewBuffer(serializedTx))
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrDeserialization.Code,
			Message: "TX decode failed",
		}
	}

	// First we add the stuff we have been given.
	// TODO(oga) really we probably should look these up with btcd anyway
	// to make sure that they match the blockchain if present.
	inputs := make(map[btcwire.OutPoint][]byte)
	scripts := make(map[string][]byte)
	for _, rti := range cmd.Inputs {
		inputSha, err := btcwire.NewShaHashFromStr(rti.Txid)
		if err != nil {
			return nil, &btcjson.Error{
				Code:    btcjson.ErrDeserialization.Code,
				Message: err.Error(),
			}
		}

		script, err := hex.DecodeString(rti.ScriptPubKey)
		if err != nil {
			return nil, &btcjson.Error{
				Code:    btcjson.ErrDeserialization.Code,
				Message: err.Error(),
			}
		}

		// redeemScript is only actually used iff the user provided
		// private keys. In which case, it is used to get the scripts
		// for signing. If the user did not provide keys then we always
		// get scripts from the wallet.
		// Empty strings are ok for this one and hex.DecodeString will
		// DTRT.
		if len(cmd.PrivKeys) != 0 {
			redeemScript, err := hex.DecodeString(rti.RedeemScript)
			if err != nil {
				return nil, &btcjson.Error{
					Code:    btcjson.ErrDeserialization.Code,
					Message: err.Error(),
				}
			}

			addr, err := btcutil.NewAddressScriptHash(redeemScript,
				cfg.Net())
			if err != nil {
				return nil, &btcjson.Error{
					Code:    btcjson.ErrDeserialization.Code,
					Message: err.Error(),
				}
			}
			scripts[addr.String()] = redeemScript
		}
		inputs[btcwire.OutPoint{
			Hash:  *inputSha,
			Index: uint32(rti.Vout),
		}] = script
	}

	// Now we go and look for any inputs that we were not provided by
	// querying btcd with getrawtransaction. We queue up a bunch of async
	// requests and will wait for replies after we have checked the rest of
	// the arguments.
	requested := make(map[btcwire.ShaHash]*pendingTx)
	for _, txIn := range msgTx.TxIn {
		// Did we get this txin from the arguments?
		if _, ok := inputs[txIn.PreviousOutpoint]; ok {
			continue
		}

		// Are we already fetching this tx? If so mark us as interested
		// in this outpoint. (N.B. that any *sane* tx will only
		// reference each outpoint once, since anything else is a double
		// spend. We don't check this ourselves to save having to scan
		// the array, it will fail later if so).
		if ptx, ok := requested[txIn.PreviousOutpoint.Hash]; ok {
			ptx.inputs = append(ptx.inputs,
				txIn.PreviousOutpoint.Index)
			continue
		}

		// Never heard of this one before, request it.
		requested[txIn.PreviousOutpoint.Hash] = &pendingTx{
			resp: GetRawTransactionAsync(CurrentServerConn(),
				&txIn.PreviousOutpoint.Hash),
			inputs: []uint32{txIn.PreviousOutpoint.Index},
		}
	}

	// Parse list of private keys, if present. If there are any keys here
	// they are the keys that we may use for signing. If empty we will
	// use any keys known to us already.
	var keys map[string]keyInfo
	if len(cmd.PrivKeys) != 0 {
		keys = make(map[string]keyInfo)

		for _, key := range cmd.PrivKeys {
			key, net, compressed, err :=
				btcutil.DecodePrivateKey(key)
			if err != nil {
				return nil, &btcjson.Error{
					Code:    btcjson.ErrDeserialization.Code,
					Message: err.Error(),
				}
			}

			if net != cfg.Net() {
				return nil, &btcjson.Error{
					Code: btcjson.ErrDeserialization.Code,
					Message: "key network doesn't match " +
						"wallet's",
				}
			}

			privk, pubk := btcec.PrivKeyFromBytes(btcec.S256(),
				key)

			var addr btcutil.Address
			if compressed {
				pkc := pubk.SerializeCompressed()
				addr, err = btcutil.NewAddressPubKey(pkc,
					cfg.Net())
				if err != nil {
					return nil, &btcjson.Error{
						Code:    btcjson.ErrDeserialization.Code,
						Message: err.Error(),
					}
				}
			} else {
				pku := pubk.SerializeUncompressed()
				addr, err = btcutil.NewAddressPubKey(pku,
					cfg.Net())
				if err != nil {
					return nil, &btcjson.Error{
						Code:    btcjson.ErrDeserialization.Code,
						Message: err.Error(),
					}
				}
			}
			keys[addr.EncodeAddress()] = keyInfo{
				key:        privk,
				compressed: compressed,
			}
		}
	}

	hashType := btcscript.SigHashAll
	if cmd.Flags != "" {
		switch cmd.Flags {
		case "ALL":
			hashType = btcscript.SigHashAll
		case "NONE":
			hashType = btcscript.SigHashNone
		case "SINGLE":
			hashType = btcscript.SigHashSingle
		case "ALL|ANYONECANPAY":
			hashType = btcscript.SigHashAll |
				btcscript.SigHashAnyOneCanPay
		case "NONE|ANYONECANPAY":
			hashType = btcscript.SigHashNone |
				btcscript.SigHashAnyOneCanPay
		case "SINGLE|ANYONECANPAY":
			hashType = btcscript.SigHashSingle |
				btcscript.SigHashAnyOneCanPay
		default:
			return nil, &btcjson.Error{
				Code:    btcjson.ErrInvalidParameter.Code,
				Message: "Invalid sighash parameter",
			}
		}
	}

	// We have checked the rest of the args. now we can collect the async
	// txs. TODO(oga) If we don't mind the possibility of wasting work we
	// could move waiting to the following loop and be slightly more
	// asynchronous.
	for txid, ptx := range requested {
		tx, err := GetRawTransactionAsyncResult(ptx.resp)
		if err != nil {
			return nil, err
		}

		for _, input := range ptx.inputs {
			if input >= uint32(len(tx.MsgTx().TxOut)) {
				return nil, &btcjson.Error{
					Code: btcjson.ErrInvalidParameter.Code,
					Message: fmt.Sprintf("input %s:%d "+
						"is not in tx", txid.String(),
						input),
				}
			}

			inputs[btcwire.OutPoint{
				Hash:  txid,
				Index: input,
			}] = tx.MsgTx().TxOut[input].PkScript
		}
	}

	// All args collected. Now we can sign all the inputs that we can.
	// `complete' denotes that we successfully signed all outputs and that
	// all scripts will run to completion. This is returned as part of the
	// reply.
	complete := true
	for i, txIn := range msgTx.TxIn {
		input, ok := inputs[txIn.PreviousOutpoint]
		if !ok {
			// failure to find previous is actually an error since
			// we failed above if we don't have all the inputs.
			return nil, &btcjson.Error{
				Code: btcjson.ErrWallet.Code,
				Message: fmt.Sprintf("%s:%d not found",
					txIn.PreviousOutpoint.Hash,
					txIn.PreviousOutpoint.Index),
			}
		}

		// Set up our callbacks that we pass to btcscript so it can
		// look up the appropriate keys and scripts by address.
		getKey := btcscript.KeyClosure(func(addr btcutil.Address) (
			*ecdsa.PrivateKey, bool, error) {
			if len(keys) != 0 {
				info, ok := keys[addr.EncodeAddress()]
				if !ok {
					return nil, false,
						errors.New("no key for address")
				}
				return info.key, info.compressed, nil
			}
			address, err := AcctMgr.Address(addr)
			if err != nil {
				return nil, false, err
			}

			pka, ok := address.(wallet.PubKeyAddress)
			if !ok {
				return nil, false, errors.New("address is not " +
					"a pubkey address")
			}

			key, err := pka.PrivKey()
			if err != nil {
				return nil, false, err
			}

			return key, pka.Compressed(), nil
		})

		getScript := btcscript.ScriptClosure(func(
			addr btcutil.Address) ([]byte, error) {
			// If keys were provided then we can only use the
			// scripts provided with our inputs, too.
			if len(keys) != 0 {
				script, ok := scripts[addr.EncodeAddress()]
				if !ok {
					return nil, errors.New("no script for " +
						"address")
				}
				return script, nil
			}
			address, err := AcctMgr.Address(addr)
			if err != nil {
				return nil, err
			}
			sa, ok := address.(wallet.ScriptAddress)
			if !ok {
				return nil, errors.New("addres is not a script" +
					" address")
			}

			// TODO(oga) we could possible speed things up further
			// by returning the addresses, class and nrequired here
			// thus avoiding recomputing them.
			return sa.Script(), nil
		})

		// SigHashSingle inputs can only be signed if there's a
		// corresponding output. However this could be already signed,
		// so we always verify the output.
		if (hashType & btcscript.SigHashSingle) == 0 ||
			i < len(msgTx.TxOut) {

			script, err := btcscript.SignTxOutput(cfg.Net(),
				msgTx, i, input, byte(hashType), getKey,
				getScript, txIn.SignatureScript)
			// Failure to sign isn't an error, it just means that
			// the tx isn't complete.
			if err != nil {
				complete = false
				continue
			}
			txIn.SignatureScript = script
		}

		// Either it was already signed or we just signed it.
		// Find out if it is completely satisfied or still needs more.
		engine, err := btcscript.NewScript(txIn.SignatureScript, input,
			i, msgTx, btcscript.ScriptBip16|
				btcscript.ScriptCanonicalSignatures)
		if err != nil || engine.Execute() != nil {
			complete = false
		}
	}

	buf := bytes.NewBuffer(nil)
	buf.Grow(msgTx.SerializeSize())

	// Buffer is the right size, this should never fail so no need to
	// come up with some synthetic error code for it.
	_ = msgTx.Serialize(buf)

	return btcjson.SignRawTransactionResult{
		Hex:      hex.EncodeToString(buf.Bytes()),
		Complete: complete,
	}, nil
}

// ValidateAddress handles the validateaddress command.
func ValidateAddress(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.ValidateAddressCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	result := btcjson.ValidateAddressResult{}
	addr, err := btcutil.DecodeAddress(cmd.Address, cfg.Net())
	if err != nil {
		return result, nil
	}

	// We could put whether or not the address is a script here,
	// by checking the type of "addr", however, the reference
	// implementation only puts that information if the script is
	// "ismine", and we follow that behaviour.
	result.Address = addr.EncodeAddress()
	result.IsValid = true

	// We can't use AcctMgr.Address() here since we also need the account
	// name.
	if account, err := AcctMgr.AccountByAddress(addr); err == nil {
		// we ignore these errors because if this call passes this can't
		// realistically fail.
		ainfo, _ := account.Address(addr)

		result.IsMine = true
		result.Account = account.name

		if pka, ok := ainfo.(wallet.PubKeyAddress); ok {
			result.IsCompressed = pka.Compressed()
			result.PubKey = pka.ExportPubKey()

		} else if sa, ok := ainfo.(wallet.ScriptAddress); ok {
			result.IsScript = true
			addresses := sa.Addresses()
			addrStrings := make([]string, len(addresses))
			for i, a := range addresses {
				addrStrings[i] = a.EncodeAddress()
			}
			result.Addresses = addrStrings
			result.Hex = hex.EncodeToString(sa.Script())

			class := sa.ScriptClass()
			// script type
			result.Script = class.String()
			if class == btcscript.MultiSigTy {
				result.SigsRequired = sa.RequiredSigs()
			}
		}
	}

	return result, nil
}

// VerifyMessage handles the verifymessage command by verifying the provided
// compact signature for the given address and message.
func VerifyMessage(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.VerifyMessageCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	addr, err := btcutil.DecodeAddress(cmd.Address, cfg.Net())
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrParse.Code,
			Message: err.Error(),
		}
	}

	// First check we know about the address and get the keys.
	ainfo, err := AcctMgr.Address(addr)
	if err != nil {
		return nil, &btcjson.ErrInvalidAddressOrKey
	}

	pka := ainfo.(wallet.PubKeyAddress)
	privkey, err := pka.PrivKey()
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	// decode base64 signature
	sig, err := base64.StdEncoding.DecodeString(cmd.Signature)
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	// Validate the signature - this just shows that it was valid at all.
	// we will compare it with the key next.
	pk, wasCompressed, err := btcec.RecoverCompact(btcec.S256(), sig,
		btcwire.DoubleSha256([]byte("Bitcoin Signed Message:\n"+
			cmd.Message)))
	if err != nil {
		return nil, &btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
	}

	// Return boolean if keys match.
	return (pk.X.Cmp(privkey.X) == 0 && pk.Y.Cmp(privkey.Y) == 0 &&
		ainfo.Compressed() == wasCompressed), nil
}

// WalletIsLocked handles the walletislocked extension request by
// returning the current lock state (false for unlocked, true for locked)
// of an account.  An error is returned if the requested account does not
// exist.
func WalletIsLocked(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcws.WalletIsLockedCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	a, err := AcctMgr.Account(cmd.Account)
	switch err {
	case nil:
		break

	case ErrNotFound:
		return nil, &btcjson.ErrWalletInvalidAccountName

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	return a.Wallet.IsLocked(), nil
}

// WalletLock handles a walletlock request by locking the all account
// wallets, returning an error if any wallet is not encrypted (for example,
// a watching-only wallet).
func WalletLock(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	if err := AcctMgr.LockWallets(); err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	return nil, nil
}

// WalletPassphrase responds to the walletpassphrase request by unlocking
// the wallet.  The decryption key is saved in the wallet until timeout
// seconds expires, after which the wallet is locked.
func WalletPassphrase(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	// Type assert icmd to access parameters.
	cmd, ok := icmd.(*btcjson.WalletPassphraseCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	if err := AcctMgr.UnlockWallets(cmd.Passphrase); err != nil {
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}

	go func(timeout int64) {
		time.Sleep(time.Second * time.Duration(timeout))
		AcctMgr.Grab()
		_ = AcctMgr.LockWallets()
		AcctMgr.Release()
	}(cmd.Timeout)

	return nil, nil
}

// WalletPassphraseChange responds to the walletpassphrasechange request
// by unlocking all accounts with the provided old passphrase, and
// re-encrypting each private key with an AES key derived from the new
// passphrase.
//
// If the old passphrase is correct and the passphrase is changed, all
// wallets will be immediately locked.
func WalletPassphraseChange(icmd btcjson.Cmd) (interface{}, *btcjson.Error) {
	cmd, ok := icmd.(*btcjson.WalletPassphraseChangeCmd)
	if !ok {
		return nil, &btcjson.ErrInternal
	}

	err := AcctMgr.ChangePassphrase([]byte(cmd.OldPassphrase),
		[]byte(cmd.NewPassphrase))
	switch err {
	case nil:
		return nil, nil

	case wallet.ErrWrongPassphrase:
		return nil, &btcjson.ErrWalletPassphraseIncorrect

	default: // all other non-nil errors
		e := btcjson.Error{
			Code:    btcjson.ErrWallet.Code,
			Message: err.Error(),
		}
		return nil, &e
	}
}

// AccountNtfn is a struct for marshalling any generic notification
// about a account for a wallet frontend.
//
// TODO(jrick): move to btcjson so it can be shared with frontends?
type AccountNtfn struct {
	Account      string      `json:"account"`
	Notification interface{} `json:"notification"`
}

// NotifyWalletLockStateChange sends a notification to all frontends
// that the wallet has just been locked or unlocked.
func NotifyWalletLockStateChange(account string, locked bool) {
	ntfn := btcws.NewWalletLockStateNtfn(account, locked)
	mntfn, _ := ntfn.MarshalJSON()
	allClients <- mntfn
}

// NotifyWalletBalance sends a confirmed account balance notification
// to a frontend.
func NotifyWalletBalance(frontend chan []byte, account string, balance float64) {
	ntfn := btcws.NewAccountBalanceNtfn(account, balance, true)
	mntfn, _ := ntfn.MarshalJSON()
	frontend <- mntfn
}

// NotifyWalletBalanceUnconfirmed sends a confirmed account balance
// notification to a frontend.
func NotifyWalletBalanceUnconfirmed(frontend chan []byte, account string, balance float64) {
	ntfn := btcws.NewAccountBalanceNtfn(account, balance, false)
	mntfn, _ := ntfn.MarshalJSON()
	frontend <- mntfn
}

// NotifyNewTxDetails sends details of a new transaction to a frontend.
func NotifyNewTxDetails(frontend chan []byte, account string,
	details btcjson.ListTransactionsResult) {

	ntfn := btcws.NewTxNtfn(account, &details)
	mntfn, _ := ntfn.MarshalJSON()
	frontend <- mntfn
}

// NotifiedRecvTxRequest is used to check whether the outpoint of
// a received transaction has already been notified due to
// arriving first in the btcd mempool.
type NotifiedRecvTxRequest struct {
	op       btcwire.OutPoint
	response chan NotifiedRecvTxResponse
}

// NotifiedRecvTxResponse is the response of a NotifiedRecvTxRequest
// request.
type NotifiedRecvTxResponse bool

// NotifiedRecvTxChans holds the channels to manage
// StoreNotifiedMempoolTxs.
var NotifiedRecvTxChans = struct {
	add, remove chan btcwire.OutPoint
	access      chan NotifiedRecvTxRequest
}{
	add:    make(chan btcwire.OutPoint),
	remove: make(chan btcwire.OutPoint),
	access: make(chan NotifiedRecvTxRequest),
}

// StoreNotifiedMempoolRecvTxs maintains a set of previously-sent
// received transaction notifications originating from the btcd
// mempool. This is used to prevent duplicate frontend transaction
// notifications once a mempool tx is mined into a block.
func StoreNotifiedMempoolRecvTxs(add, remove chan btcwire.OutPoint,
	access chan NotifiedRecvTxRequest) {

	m := make(map[btcwire.OutPoint]struct{})
	for {
		select {
		case op := <-add:
			m[op] = struct{}{}

		case op := <-remove:
			if _, ok := m[op]; ok {
				delete(m, op)
			}

		case req := <-access:
			_, ok := m[req.op]
			req.response <- NotifiedRecvTxResponse(ok)
		}
	}
}

// NotifyBalanceSyncerChans holds channels for accessing
// the NotifyBalanceSyncer goroutine.
var NotifyBalanceSyncerChans = struct {
	add    chan NotifyBalanceWorker
	remove chan btcwire.ShaHash
	access chan NotifyBalanceRequest
}{
	add:    make(chan NotifyBalanceWorker),
	remove: make(chan btcwire.ShaHash),
	access: make(chan NotifyBalanceRequest),
}

// NotifyBalanceWorker holds a block hash to add a worker to
// NotifyBalanceSyncer and uses a chan to returns the WaitGroup
// which should be decremented with Done after the worker is finished.
type NotifyBalanceWorker struct {
	block btcwire.ShaHash
	wg    chan *sync.WaitGroup
}

// NotifyBalanceRequest is used by the blockconnected notification handler
// to access and wait on the the WaitGroup for workers currently processing
// transactions for a block.  If no handlers have been added, a nil
// WaitGroup is returned.
type NotifyBalanceRequest struct {
	block btcwire.ShaHash
	wg    chan *sync.WaitGroup
}

// NotifyBalanceSyncer maintains a map of block hashes to WaitGroups
// for worker goroutines that must finish before it is safe to notify
// frontends of a new balance in the blockconnected notification handler.
func NotifyBalanceSyncer(add chan NotifyBalanceWorker,
	remove chan btcwire.ShaHash,
	access chan NotifyBalanceRequest) {

	m := make(map[btcwire.ShaHash]*sync.WaitGroup)

	for {
		select {
		case worker := <-add:
			wg, ok := m[worker.block]
			if !ok {
				wg = &sync.WaitGroup{}
				m[worker.block] = wg
			}
			wg.Add(1)
			m[worker.block] = wg
			worker.wg <- wg

		case block := <-remove:
			if _, ok := m[block]; ok {
				delete(m, block)
			}

		case req := <-access:
			req.wg <- m[req.block]
		}
	}
}
