package generator

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	gethstate "github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
	"github.com/kkrt-labs/go-utils/log"
	"github.com/kkrt-labs/go-utils/tag"
	"github.com/kkrt-labs/zk-pig/src/ethereum"
	"github.com/kkrt-labs/zk-pig/src/ethereum/evm"
	input "github.com/kkrt-labs/zk-pig/src/prover-input"
	"go.uber.org/zap"
)

// Executor is the interface for EVM execution on provable inputs.
// It runs a full "execution + final state validation" of the block
// It is primarily meant to validate that the provable inputs are correct and enable proper EVM execution.
type Executor interface {
	// Execute runs a full EVM block execution on provable inputs
	Execute(ctx context.Context, inputs *input.ProverInput) (*core.ProcessResult, error)
}

type executor struct{}

// NewExecutor creates a new instance of the BaseExecutor.
func NewExecutor() Executor {
	return &executor{}
}

// Execute runs the ProvableBlockInputs data for the EVM prover engine.
func (e *executor) Execute(ctx context.Context, inputs *input.ProverInput) (*core.ProcessResult, error) {
	if len(inputs.Blocks) == 0 {
		return nil, fmt.Errorf("no blocks provided")
	}

	block := inputs.Blocks[0]

	ctx = tag.WithComponent(ctx, "execute")
	ctx = tag.WithTags(
		ctx,
		tag.Key("chain.id").String(inputs.ChainConfig.ChainID.String()),
		tag.Key("block.number").Int64(block.Header.Number.Int64()),
		tag.Key("block.hash").String(block.Header.Hash().Hex()),
	)

	res, err := e.execute(ctx, inputs)
	if err != nil {
		log.LoggerFromContext(ctx).Error("Provable execution failed", zap.Error(err))
		return res, err
	}

	log.LoggerFromContext(ctx).Info("Provable execution succeeded")

	return res, err
}

type executorContext struct {
	ctx     context.Context
	stateDB gethstate.Database
	hc      *core.HeaderChain
}

func (e *executor) execute(ctx context.Context, inputs *input.ProverInput) (*core.ProcessResult, error) {
	log.LoggerFromContext(ctx).Info("Process provable execution...")

	execCtx, err := e.prepareContext(ctx, inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare execution context: %v", err)
	}

	e.preparePreState(execCtx, inputs)

	execParams, err := e.prepareExecParams(execCtx, inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare execution exec params: %v", err)
	}

	return e.execEVM(execCtx, execParams)
}

func (e *executor) prepareContext(ctx context.Context, inputs *input.ProverInput) (*executorContext, error) {
	log.LoggerFromContext(ctx).Debug("Prepare context...")

	// --- Create necessary database and chain instances ---
	db := rawdb.NewMemoryDatabase()
	trieDB := triedb.NewDatabase(db, &triedb.Config{HashDB: &hashdb.Config{}})
	stateDB := gethstate.NewDatabase(trieDB, nil) // We use a modified trie database to track trie modifications

	hc, err := ethereum.NewChain(inputs.ChainConfig, stateDB)
	if err != nil {
		return nil, fmt.Errorf("failed to create chain: %v", err)
	}

	return &executorContext{
		ctx:     ctx,
		stateDB: stateDB,
		hc:      hc,
	}, nil
}

func (e *executor) preparePreState(ctx *executorContext, inputs *input.ProverInput) {
	log.LoggerFromContext(ctx.ctx).Info("Prepare pre-state...")

	// -- Preload the ancestors of the block into database ---
	ethereum.WriteHeaders(ctx.stateDB.TrieDB().Disk(), inputs.Witness.Ancestors...)

	// --- Preload the account bytecodes into the database ---
	codes := make([][]byte, 0)
	for _, code := range inputs.Witness.Codes {
		codes = append(codes, code)
	}
	ethereum.WriteCodes(ctx.stateDB.TrieDB().Disk(), codes...)

	// -- Preload the pre-state nodes to database ---
	nodes := make([][]byte, 0)
	for _, node := range inputs.Witness.State {
		nodes = append(nodes, node)
	}
	ethereum.WriteNodesToHashDB(ctx.stateDB.TrieDB().Disk(), nodes...)
}

func (e *executor) prepareExecParams(ctx *executorContext, inputs *input.ProverInput) (*evm.ExecParams, error) {
	if len(inputs.Blocks) == 0 {
		return nil, fmt.Errorf("no blocks provided")
	}

	log.LoggerFromContext(ctx.ctx).Debug("Prepare execution parameters...")

	if len(inputs.Witness.Ancestors) == 0 {
		return nil, fmt.Errorf("no ancestors provided")
	}

	parentHeader := inputs.Witness.Ancestors[0]

	if parentHeader == nil || parentHeader.Hash() == inputs.Blocks[0].Header.Hash() {
		return nil, fmt.Errorf("first ancestor must be the parent of the first block")
	}

	preState, err := gethstate.New(parentHeader.Root, ctx.stateDB)
	if err != nil {
		return nil, fmt.Errorf("failed to create pre-state from parent root %v: %v", parentHeader.Root, err)
	}

	return &evm.ExecParams{
		VMConfig: &vm.Config{
			StatelessSelfValidation: true,
		},
		Block:    inputs.Blocks[0].Block(),
		Validate: true, // We validate the block execution to ensure the result and final state are correct
		Chain:    ctx.hc,
		State:    preState,
	}, nil
}

func (e *executor) execEVM(ctx *executorContext, execParams *evm.ExecParams) (*core.ProcessResult, error) {
	log.LoggerFromContext(ctx.ctx).Info("Execute EVM...")

	res, err := evm.ExecutorWithTags("evm")(evm.ExecutorWithLog()(evm.NewExecutor())).Execute(ctx.ctx, execParams)
	if err != nil {
		return res, fmt.Errorf("failed to execute block: %v", err)
	}

	return res, nil
}
