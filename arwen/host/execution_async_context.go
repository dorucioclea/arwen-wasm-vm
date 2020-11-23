package host

import (
	"encoding/json"
	"math/big"

	"github.com/ElrondNetwork/arwen-wasm-vm/arwen"
	"github.com/ElrondNetwork/arwen-wasm-vm/math"
	"github.com/ElrondNetwork/elrond-go/core/vmcommon"
)

// executeCurrentAsyncContext is the entry-point of the async calling mechanism; it is
// called by host.ExecuteOnDestContext() and host.callSCMethod(). When
// executeCurrentAsyncContext() finishes, there should be no remaining AsyncCalls that
// can be executed synchronously, and all AsyncCalls that require asynchronous
// execution must already have corresponding entries in
// vmOutput.OutputAccounts, to be dispatched across shards.
//
// executeCurrentAsyncContext() does NOT handle the callbacks of cross-shard
// AsyncCalls. See postprocessCrossShardCallback() for that
//
// Note that executeCurrentAsyncContext() is mutually recursive with
// host.ExecuteOnDestContext(), because synchronous AsyncCalls are executed
// with host.ExecuteOnDestContext(), which, in turn, calls
// host.executeCurrentAsyncContext() to resolve AsyncCalls generated by the
// AsyncCalls, and so on.
func (host *vmHost) executeCurrentAsyncContext() error {
	runtime := host.Runtime()
	asyncContext := runtime.GetAsyncContext()

	if asyncContext.IsCompleted() {
		return nil
	}

	// Step 1: execute all AsyncCalls that can be executed synchronously
	// (includes smart contracts and built-in functions in the same shard)
	err := host.setupAsyncCallsGas(asyncContext)
	if err != nil {
		return err
	}

	for groupIndex, group := range asyncContext.AsyncCallGroups {
		// Execute the call group strictly synchronously (no asynchronous calls allowed)
		err := host.executeAsyncCallGroup(group, true)
		if err != nil {
			return err
		}

		if group.IsCompleted() {
			asyncContext.DeleteAsyncCallGroup(groupIndex)
		}
	}

	// Step 2: redistribute unspent gas; then, in one combined step, do the
	// following:
	// * locally execute built-in functions with cross-shard
	//   destinations, whereby the cross-shard OutputAccount entries are generated
	// * call host.sendAsyncCallCrossShard() for each pending AsyncCall, to
	//   generate the corresponding cross-shard OutputAccount entries
	err = host.setupAsyncCallsGas(asyncContext)
	if err != nil {
		return err
	}

	for _, group := range asyncContext.AsyncCallGroups {
		// Execute the call group allowing asynchronous (cross-shard) calls as well
		err = host.executeAsyncCallGroup(group, false)
		if err != nil {
			return err
		}
	}

	asyncContext.DeleteAsyncCallGroupByID(arwen.LegacyAsyncCallGroupID)

	err = host.saveAsyncContext(asyncContext)
	if err != nil {
		return err
	}

	return nil
}

// TODO split into two different functions, for sync execution and async
// execution, and remove parameter syncExecutionOnly
func (host *vmHost) executeAsyncCallGroup(
	group *arwen.AsyncCallGroup,
	syncExecutionOnly bool,
) error {
	for _, asyncCall := range group.AsyncCalls {
		err := host.executeAsyncCall(asyncCall, syncExecutionOnly)
		if err != nil {
			return err
		}
	}

	group.DeleteCompletedAsyncCalls()

	// If ALL the AsyncCalls in the AsyncCallGroup were executed synchronously,
	// then the AsyncCallGroup can have its callback executed.
	if group.IsCompleted() {
		// TODO reenable this, after allowing a gas limit for it and deciding what
		// arguments it receives (this method is currently a NOP and returns nil)
		return host.executeAsyncCallGroupCallback(group)
	}

	return nil
}

func (host *vmHost) executeAsyncCall(
	asyncCall *arwen.AsyncCall,
	syncExecutionOnly bool,
) error {
	execMode, err := host.determineAsyncCallExecutionMode(asyncCall)
	if err != nil {
		return err
	}

	if execMode == arwen.SyncExecution {
		vmOutput, err := host.executeSyncCall(asyncCall)

		// The vmOutput instance returned by host.executeSyncCall() is never nil,
		// by design. Using it without checking for err is safe here.
		asyncCall.UpdateStatus(vmOutput.ReturnCode)

		// TODO host.executeSyncCallback() returns a vmOutput produced by executing
		// the callback. Information from this vmOutput should be preserved in the
		// pending AsyncCallGroup, and made available to the callback of the
		// AsyncCallGroup (currently not implemented).
		callbackVMOutput, callbackErr := host.executeSyncCallback(asyncCall, vmOutput, err)
		host.finishSyncExecution(callbackVMOutput, callbackErr)
		return nil
	}

	if syncExecutionOnly {
		return nil
	}

	if execMode == arwen.AsyncBuiltinFunc {
		// Built-in functions will handle cross-shard calls themselves, by
		// generating entries in vmOutput.OutputAccounts, but they need to be
		// executed synchronously to do that. It is not necessary to call
		// sendAsyncCallCrossShard(). The vmOutput produced by the built-in
		// function, containing the cross-shard call, has ALREADY been merged into
		// the main output by the inner call to host.ExecuteOnDestContext().  The
		// status of the AsyncCall is not updated here - it will be updated by
		// postprocessCrossShardCallback(), when the cross-shard call returns.
		vmOutput, err := host.executeSyncCall(asyncCall)
		if err != nil {
			return err
		}

		if vmOutput.ReturnCode != vmcommon.Ok {
			asyncCall.UpdateStatus(vmOutput.ReturnCode)
			callbackVMOutput, callbackErr := host.executeSyncCallback(asyncCall, vmOutput, err)
			host.finishSyncExecution(callbackVMOutput, callbackErr)
		}

		return nil
	}

	if execMode == arwen.AsyncUnknown {
		return host.sendAsyncCallCrossShard(asyncCall)
	}

	return nil
}

func (host *vmHost) determineAsyncCallExecutionMode(asyncCall *arwen.AsyncCall) (arwen.AsyncCallExecutionMode, error) {
	return host.determineExecutionMode(asyncCall.Destination, asyncCall.Data)
}

func (host *vmHost) determineExecutionMode(destination []byte, data []byte) (arwen.AsyncCallExecutionMode, error) {
	runtime := host.Runtime()
	blockchain := host.Blockchain()

	// If ArgParser cannot read the Data field, then this is neither a SC call,
	// nor a built-in function call.
	functionName, _, err := host.CallArgsParser().ParseData(string(data))
	if err != nil {
		return arwen.AsyncUnknown, err
	}

	shardOfSC := blockchain.GetShardOfAddress(runtime.GetSCAddress())
	shardOfDest := blockchain.GetShardOfAddress(destination)
	sameShard := shardOfSC == shardOfDest

	if sameShard {
		return arwen.SyncExecution, nil
	}

	if host.IsBuiltinFunctionName(functionName) {
		return arwen.AsyncBuiltinFunc, nil
	}

	return arwen.AsyncUnknown, nil
}

func (host *vmHost) executeSyncCall(asyncCall *arwen.AsyncCall) (*vmcommon.VMOutput, error) {
	destinationCallInput, err := host.createSyncCallInput(asyncCall)
	if err != nil {
		return nil, err
	}

	return host.ExecuteOnDestContext(destinationCallInput)
}

func (host *vmHost) executeSyncCallback(
	asyncCall *arwen.AsyncCall,
	vmOutput *vmcommon.VMOutput,
	err error,
) (*vmcommon.VMOutput, error) {

	callbackInput, err := host.createSyncCallbackInput(asyncCall, vmOutput, err)
	if err != nil {
		return nil, err
	}

	return host.ExecuteOnDestContext(callbackInput)
}

func (host *vmHost) executeAsyncCallGroupCallback(group *arwen.AsyncCallGroup) error {
	// TODO implement this
	return nil
}

func (host *vmHost) createSyncCallInput(asyncCall arwen.AsyncCallHandler) (*vmcommon.ContractCallInput, error) {
	runtime := host.Runtime()
	sender := runtime.GetSCAddress()

	function, arguments, err := host.CallArgsParser().ParseData(string(asyncCall.GetData()))
	if err != nil {
		return nil, err
	}

	gasLimit := asyncCall.GetGasLimit()
	gasToUse := host.Metering().GasSchedule().ElrondAPICost.AsyncCallStep
	if gasLimit <= gasToUse {
		return nil, arwen.ErrNotEnoughGas
	}
	gasLimit -= gasToUse

	contractCallInput := &vmcommon.ContractCallInput{
		VMInput: vmcommon.VMInput{
			CallerAddr:     sender,
			Arguments:      arguments,
			CallValue:      big.NewInt(0).SetBytes(asyncCall.GetValueBytes()),
			CallType:       vmcommon.AsynchronousCall,
			GasPrice:       runtime.GetVMInput().GasPrice,
			GasProvided:    gasLimit,
			CurrentTxHash:  runtime.GetCurrentTxHash(),
			OriginalTxHash: runtime.GetOriginalTxHash(),
			PrevTxHash:     runtime.GetPrevTxHash(),
		},
		RecipientAddr: asyncCall.GetDestination(),
		Function:      function,
	}

	return contractCallInput, nil
}

func (host *vmHost) createSyncCallbackInput(
	asyncCall *arwen.AsyncCall,
	vmOutput *vmcommon.VMOutput,
	destinationErr error,
) (*vmcommon.ContractCallInput, error) {
	metering := host.Metering()
	runtime := host.Runtime()

	// always provide return code as the first argument to callback function
	arguments := [][]byte{
		big.NewInt(int64(vmOutput.ReturnCode)).Bytes(),
	}
	if destinationErr == nil {
		// when execution went Ok, callBack arguments are:
		// [0, result1, result2, ....]
		arguments = append(arguments, vmOutput.ReturnData...)
	} else {
		// when execution returned error, callBack arguments are:
		// [error code, error message]
		arguments = append(arguments, []byte(vmOutput.ReturnMessage))
	}

	callbackFunction := asyncCall.GetCallbackName()

	gasLimit := vmOutput.GasRemaining + asyncCall.GetGasLocked()
	dataLength := host.computeDataLengthFromArguments(callbackFunction, arguments)

	gasToUse := metering.GasSchedule().ElrondAPICost.AsyncCallStep
	gasToUse += metering.GasSchedule().BaseOperationCost.DataCopyPerByte * uint64(dataLength)
	if gasLimit <= gasToUse {
		return nil, arwen.ErrNotEnoughGas
	}
	gasLimit -= gasToUse

	// Return to the sender SC, calling its specified callback method.
	contractCallInput := &vmcommon.ContractCallInput{
		VMInput: vmcommon.VMInput{
			CallerAddr:     asyncCall.Destination,
			Arguments:      arguments,
			CallValue:      big.NewInt(0),
			CallType:       vmcommon.AsynchronousCallBack,
			GasPrice:       runtime.GetVMInput().GasPrice,
			GasProvided:    gasLimit,
			CurrentTxHash:  runtime.GetCurrentTxHash(),
			OriginalTxHash: runtime.GetOriginalTxHash(),
			PrevTxHash:     runtime.GetPrevTxHash(),
		},
		RecipientAddr: runtime.GetSCAddress(),
		Function:      callbackFunction,
	}

	return contractCallInput, nil
}

func (host *vmHost) sendAsyncCallCrossShard(asyncCall arwen.AsyncCallHandler) error {
	runtime := host.Runtime()
	output := host.Output()

	err := output.Transfer(
		asyncCall.GetDestination(),
		runtime.GetSCAddress(),
		asyncCall.GetGasLimit(),
		asyncCall.GetGasLocked(),
		big.NewInt(0).SetBytes(asyncCall.GetValueBytes()),
		asyncCall.GetData(),
		vmcommon.AsynchronousCall,
	)
	if err != nil {
		metering := host.Metering()
		metering.UseGas(metering.GasLeft())
		runtime.FailExecution(err)
		return err
	}

	return nil
}

/**
 * setupAsyncCallsGas sets the gasLimit for each async call with the amount of gas provided by the
 *  SC developer. The remaining gas is split between the async calls where the developer
 *  did not specify any gas amount
 */
func (host *vmHost) setupAsyncCallsGas(asyncContext *arwen.AsyncContext) error {
	gasLeft := host.Metering().GasLeft()
	gasNeeded := uint64(0)
	callsWithZeroGas := uint64(0)

	for _, group := range asyncContext.AsyncCallGroups {
		for _, asyncCall := range group.AsyncCalls {
			var err error
			gasNeeded, err = math.AddUint64(gasNeeded, asyncCall.ProvidedGas)
			if err != nil {
				return err
			}

			if gasNeeded > gasLeft {
				return arwen.ErrNotEnoughGas
			}

			if asyncCall.ProvidedGas == 0 {
				callsWithZeroGas++
				continue
			}

			asyncCall.GasLimit = asyncCall.ProvidedGas
		}
	}

	if callsWithZeroGas == 0 {
		return nil
	}

	if gasLeft <= gasNeeded {
		return arwen.ErrNotEnoughGas
	}

	gasShare := (gasLeft - gasNeeded) / callsWithZeroGas
	for _, group := range asyncContext.AsyncCallGroups {
		for _, asyncCall := range group.AsyncCalls {
			if asyncCall.ProvidedGas == 0 {
				asyncCall.GasLimit = gasShare
			}
		}
	}

	return nil
}

func (host *vmHost) finishSyncExecution(vmOutput *vmcommon.VMOutput, err error) {
	if err == nil {
		return
	}

	runtime := host.Runtime()
	output := host.Output()

	runtime.GetVMInput().GasProvided = 0

	if vmOutput == nil {
		vmOutput = output.CreateVMOutputInCaseOfError(err)
	}

	output.SetReturnMessage(vmOutput.ReturnMessage)

	output.Finish([]byte(vmOutput.ReturnCode.String()))
	output.Finish(runtime.GetCurrentTxHash())
}

/**
 * saveAsyncContext takes a list of pending async calls and save them to storage so the info will be available on callback
 */
func (host *vmHost) saveAsyncContext(asyncContext *arwen.AsyncContext) error {
	if len(asyncContext.AsyncCallGroups) == 0 {
		return nil
	}

	storage := host.Storage()
	runtime := host.Runtime()

	asyncCallStorageKey := arwen.CustomStorageKey(arwen.AsyncDataPrefix, runtime.GetPrevTxHash())
	data, err := json.Marshal(asyncContext)
	if err != nil {
		return err
	}

	_, err = storage.SetStorage(asyncCallStorageKey, data)
	if err != nil {
		return err
	}

	return nil
}

func (host *vmHost) computeDataLengthFromArguments(function string, arguments [][]byte) int {
	// Calculate what length would the Data field have, were it of the
	// form "callback@arg1@arg4...

	// TODO this needs tests, especially for the case when the arguments slice
	// contains an empty []byte
	numSeparators := len(arguments)
	dataLength := len(function) + numSeparators
	for _, element := range arguments {
		dataLength += len(element)
	}

	return dataLength
}