// Package commp allows calculating a Filecoin Unsealed Commitment (commP/commD)
// given a bytestream. It is implemented as a standard hash.Hash() interface, with
// the entire padding and treebuilding algorithm written in golang.
//
// The returned digest is a 32-byte raw commitment payload. Use something like
// https://pkg.go.dev/github.com/filecoin-project/go-fil-commcid#DataCommitmentV1ToCID
// in order to convert it to a proper cid.Cid.
//
// The output of this library is 100% identical to https://github.com/filecoin-project/filecoin-ffi/blob/d82899449741ce19/proofs.go#L177-L196
package commp

import (
	"fmt"
	"hash"
	"math/bits"
	"sync"

	sha256simd "github.com/minio/sha256-simd"
	"golang.org/x/xerrors"
)

// Calc is an implementation of a commP "hash" calculator, implementing the
// familiar hash.Hash interface. The zero-value of this object is ready to
// accept Write()s without further initialization.
type Calc struct {
	state
	blockSize int
	mu sync.Mutex
}
type state struct {
	bytesConsumed uint64
	layerQueues   [MaxLayers + 2]chan []byte // one extra layer for the initial leaves, one more for the dummy never-to-use channel
	resultCommP   chan []byte
	carry         []byte
}

var _ hash.Hash = &Calc{} // make sure we are hash.Hash compliant

// MaxLayers is the current maximum height of the rust-fil-proofs proving tree.
const MaxLayers = uint(31) // result of log2( 64 GiB / 32 )

// MaxPiecePayload is the maximum amount of data that one can Write() to the
// Calc object, before needing to derive a Digest(). Constrained by the value
// of MaxLayers.
const MaxPiecePayload = uint64(127 * (1 << (5 + MaxLayers - 7)))

// MinPiecePayload is the smallest amount of data for which FR32 padding has
// a defined result. It is not possible to derive a Digest() before Write()ing
// at least this amount of bytes.
const MinPiecePayload = uint64(65)

var (
	layerQueueDepth   = 256 // SANCHECK: too much? too little? can't think this through right now...
	shaPool           = sync.Pool{New: func() interface{} { return sha256simd.New() }}
	stackedNulPadding [MaxLayers][]byte
)

// initialize the nul padding stack (cheap to do upfront, just MaxLayers loops)
func init() {
	h := shaPool.Get().(hash.Hash)
	defer shaPool.Put(h)

	stackedNulPadding[0] = make([]byte, 32)
	for i := uint(1); i < MaxLayers; i++ {
		h.Reset()
		h.Write(stackedNulPadding[i-1]) // yes, got to
		h.Write(stackedNulPadding[i-1]) // do it twice
		stackedNulPadding[i] = h.Sum(make([]byte, 0, 32))
		stackedNulPadding[i][31] &= 0x3F
	}
}

// BlockSize is the amount of bytes consumed by the commP algorithm in one go.
// Write()ing data in multiples of BlockSize would obviate the need to maintain
// an internal carry buffer. The BlockSize of this module is 127 bytes.
func (cp *Calc) BlockSize() int { return 127 }

// Size is the amount of bytes returned on Sum()/Digest(), which is 32 bytes
// for this module.
func (cp *Calc) Size() int { return 32 }

// Reset re-initializes the accumulator object, clearing its state and
// terminating all background goroutines. It is safe to Reset() an accumulator
// in any state.
func (cp *Calc) Reset() {
	cp.mu.Lock()
	if cp.bytesConsumed != 0 {
		// we are resetting without digesting: close everything out to terminate
		// the layer workers
		close(cp.layerQueues[0])
		<-cp.resultCommP
	}
	cp.state = state{} // reset
	cp.mu.Unlock()
}

// Sum is a thin wrapper around Digest() and is provided solely to satisfy
// the hash.Hash interface. It panics on errors returned from Digest().
// Note that unlike classic (hash.Hash).Sum(), calling this method is
// destructive: the internal state is reset and all goroutines kicked off
// by Write() are terminated.
func (cp *Calc) Sum(buf []byte) []byte {
	commP, _, err := cp.Digest()
	if err != nil {
		panic(err)
	}
	return append(buf, commP...)
}

// Digest collapses the internal hash state and returns the resulting raw 32
// bytes of commP and the padded piece size, or alternatively an error in
// case of insufficient accumulated state. On success invokes Reset(), which
// terminates all goroutines kicked off by Write().
func (cp *Calc) Digest() (commP []byte, paddedPieceSize uint64, err error) {
	cp.mu.Lock()

	defer func() {
		// reset only if we did succeed
		if err == nil {
			cp.state = state{}
		}
		cp.mu.Unlock()
	}()

	if cp.bytesConsumed < MinPiecePayload {
		err = xerrors.Errorf(
			"insufficient state accumulated: commP is not defined for inputs shorter than %d bytes, but only %d processed so far",
			MinPiecePayload, cp.bytesConsumed,
		)
		return
	}

	// If any, flush remaining bytes padded up with zeroes
	if len(cp.carry) > 0 {
		if mod := len(cp.carry) % 127; mod != 0 {
			cp.carry = append(cp.carry, make([]byte, 127-mod)...)
		}
		for len(cp.carry) > 0{
			if unprocessed := cp.digestLeadingBytes(cp.carry[:127]); unprocessed != 0 {
				panic(fmt.Sprintf("Unexpected number of unprocessed bytes : %d ! ", unprocessed))
			}
			cp.carry = cp.carry[127:]
		}
	}

	// This is how we signal to the bottom of the stack that we are done
	// which in turn collapses the rest all the way to resultCommP
	close(cp.layerQueues[0])

	// hacky round-up-to-next-pow2
	paddedPieceSize = ((cp.bytesConsumed + 126) / 127 * 128) // why is 6 afraid of 7...?
	if bits.OnesCount64(paddedPieceSize) != 1 {
		paddedPieceSize = 1 << uint(64-bits.LeadingZeros64(paddedPieceSize))
	}

	return <-cp.resultCommP, paddedPieceSize, nil
}

// Write adds bytes to the accumulator, for a subsequent Digest(). Upon the
// first call of this method a few goroutines are started in the background to
// service each layer of the digest tower. If you wrote some data and then
// decide to abandon the object without invoking Digest(), you need to call
// Reset() to terminate all remaining background workers. Unlike a typical
// (hash.Hash).Write, calling this method can return an error when the total
// amount of bytes is about to go over the maximum currently supported by
// Filecoin.
func (cp *Calc) Write(input []byte) (int, error) {
	inputSize := len(input)
	if inputSize == 0 {
		return 0, nil
	}

	cp.mu.Lock()
	defer cp.mu.Unlock()

	if cp.bytesConsumed+uint64(inputSize) > MaxPiecePayload {
		return 0, xerrors.Errorf(
			"writing %d bytes to the accumulator would overflow the maximum supported unpadded piece size %d",
			len(input), MaxPiecePayload,
		)
	}

	// just starting: initialize internal state, start first background layer-goroutine
	if cp.bytesConsumed == 0 {
		cp.blockSize = 127 * 8
		cp.carry = make([]byte, 0, cp.blockSize)
		cp.resultCommP = make(chan []byte, 1)
		cp.layerQueues[0] = make(chan []byte, layerQueueDepth)
		cp.addLayer(0)
	}

	cp.bytesConsumed += uint64(inputSize)

	carrySize := len(cp.carry)
	if carrySize > 0 {

		// super short Write - just carry it
		if carrySize+inputSize < cp.blockSize {
			cp.carry = append(cp.carry, input...)
			return inputSize, nil
		}

		cp.carry = append(cp.carry, input[:cp.blockSize-carrySize]...)
		input = input[cp.blockSize-carrySize:]

		if unprocessed := cp.digestLeadingBytes(cp.carry); unprocessed != 0 {
			panic(fmt.Sprintf("Unexpected number of unprocessed bytes in carry : %d ! ", unprocessed))
		}
		cp.carry = cp.carry[:0]
	}

	for len(input) >= cp.blockSize {
		//fmt.Printf("inputSize : %d\n", len(input))
		unprocessed := cp.digestLeadingBytes(input[:cp.blockSize])
		input = input[cp.blockSize - unprocessed:]
	}

	if len(input) > 0 {
		cp.carry = cp.carry[:len(input)]
		copy(cp.carry, input)
	}

	return inputSize, nil
}

func (cp *Calc) digestLeadingBytes(inSlab []byte) (countUnprocessedBytes int) {

	// Holds this round's shifts of the original 127 bytes plus the 6 bit overflow
	// at the end of the expansion cycle. We *do not* reuse this array: it is
	// being fed piece-wise to hash254Into which in turn reuses it for the result
	if len(inSlab) < 127 {
		panic("Input shorter than 127 bytes")
	}
	outSize := len(inSlab) / 127 * 128
	if bits.OnesCount64(uint64(outSize)) != 1 {
		outSize = 1 << uint(63 - bits.LeadingZeros64(uint64(outSize)))
	}
	outSlab := make([]byte, outSize)

	//fmt.Printf("inSize : %d, outsize: %d\n", len(inSlab), outSize)

	for j := 0; j < outSize/128; j++ {
		// Cycle over four(4) 31-byte groups, leaving 1 byte in between:
		// 31 + 1 + 31 + 1 + 31 + 1 + 31 = 127
		input := inSlab[j*127 : (j+1)*127]
		expander := outSlab[j*128 : (j+1)*128]
		inputPlus1, expanderPlus1 := input[1:], expander[1:]

		// First 31 bytes + 6 bits are taken as-is (trimmed later)
		// Note that copying them into the expansion buffer is mandatory:
		// we will be feeding it to the workers which reuse the bottom half
		// of the chunk for the result
		copy(expander[:], input[:32])

		// first 2-bit "shim" forced into the otherwise identical bitstream
		expander[31] &= 0x3F

		//  In: {{ C[7] C[6] }} X[7] X[6] X[5] X[4] X[3] X[2] X[1] X[0] Y[7] Y[6] Y[5] Y[4] Y[3] Y[2] Y[1] Y[0] Z[7] Z[6] Z[5]...
		// Out:                 X[5] X[4] X[3] X[2] X[1] X[0] C[7] C[6] Y[5] Y[4] Y[3] Y[2] Y[1] Y[0] X[7] X[6] Z[5] Z[4] Z[3]...
		for i := 31; i < 63; i++ {
			expanderPlus1[i] = inputPlus1[i]<<2 | input[i]>>6
		}

		// next 2-bit shim
		expander[63] &= 0x3F

		//  In: {{ C[7] C[6] C[5] C[4] }} X[7] X[6] X[5] X[4] X[3] X[2] X[1] X[0] Y[7] Y[6] Y[5] Y[4] Y[3] Y[2] Y[1] Y[0] Z[7] Z[6] Z[5]...
		// Out:                           X[3] X[2] X[1] X[0] C[7] C[6] C[5] C[4] Y[3] Y[2] Y[1] Y[0] X[7] X[6] X[5] X[4] Z[3] Z[2] Z[1]...
		for i := 63; i < 95; i++ {
			expanderPlus1[i] = inputPlus1[i]<<4 | input[i]>>4
		}

		// next 2-bit shim
		expander[95] &= 0x3F

		//  In: {{ C[7] C[6] C[5] C[4] C[3] C[2] }} X[7] X[6] X[5] X[4] X[3] X[2] X[1] X[0] Y[7] Y[6] Y[5] Y[4] Y[3] Y[2] Y[1] Y[0] Z[7] Z[6] Z[5]...
		// Out:                                     X[1] X[0] C[7] C[6] C[5] C[4] C[3] C[2] Y[1] Y[0] X[7] X[6] X[5] X[4] X[3] X[2] Z[1] Z[0] Y[7]...
		for i := 95; i < 126; i++ {
			expanderPlus1[i] = inputPlus1[i]<<6 | input[i]>>2
		}

		// the final 6 bit remainder is exactly the value of the last expanded byte
		expander[127] = input[126] >> 2
	}

	// fmt.Printf("%X", outSlab)
	cp.layerQueues[0] <- outSlab
	//fmt.Printf("unprocessed : %d \n", len(inSlab) -  outSize / 128 * 127)
	return len(inSlab) -  outSize / 128 * 127
}

func (cp *Calc) addLayer(myIdx uint) {
	// the next layer channel, which we might *not* use
	if cp.layerQueues[myIdx+1] != nil {
		panic("addLayer called more than once with identical idx argument")
	}
	cp.layerQueues[myIdx+1] = make(chan []byte, layerQueueDepth)
	//fmt.Printf("creating worker for layer : %d \n", myIdx)

	go func() {
		var chunkHold []byte

		for {
			slab, queueIsOpen := <-cp.layerQueues[myIdx]

			//fmt.Printf("--layerId: %d \t slab length: %d \t queueIsOpen: %t\n", myIdx, len(slab), queueIsOpen)


			// the dream is collapsing
			if !queueIsOpen {

				// I am last
				if myIdx == MaxLayers || cp.layerQueues[myIdx+2] == nil {
					//spew.Dump(cp.layerQueues)
					//fmt.Printf("top layer idx : %d\n", myIdx)
					cp.resultCommP <- chunkHold[0:32]
					return
				}

				if chunkHold != nil {
					copy(chunkHold[32:64], stackedNulPadding[myIdx])
					cp.hashSlab254(0, chunkHold[0:64], cp.layerQueues[myIdx+1])
				}

				// signal the next in line that they are done too
				close(cp.layerQueues[myIdx+1])
				return
			}

			if len(slab) > 1<<(5+myIdx) {
				//fmt.Printf("inside %d \n", myIdx)
				cp.hashSlab254(myIdx, slab, cp.layerQueues[myIdx+1])
				if cp.layerQueues[myIdx+2] == nil {
					cp.addLayer(myIdx + 1)
				}
				continue
			}
			if chunkHold == nil {
				chunkHold = slab[0:32]
			} else {

				//fmt.Printf("chunkHold : %x, layer : %d\n", chunkHold, myIdx)
				// We are last right now
				// n.b. we will not blow out of the preallocated layerQueues array,
				// as we disallow Write()s above a certain threshold
				if cp.layerQueues[myIdx+2] == nil {
					cp.addLayer(myIdx + 1)
				}
				copy(chunkHold[32:64], slab[0:32])
				cp.hashSlab254(0, chunkHold[0:64], cp.layerQueues[myIdx+1])
				chunkHold = nil
			}
		}
	}()
}

func (cp *Calc) hashSlab254(layerIdx uint, slab []byte, target chan []byte) {
	//fmt.Printf("layerId: %d \t slab length: %d \n", layerIdx, len(slab))

	stride := 1 << (5+layerIdx)
	if len(slab) < stride {
		panic(fmt.Sprintf("wtf layerId: %d \t slab length: %d \n", layerIdx, len(slab)))
	}
	var wg sync.WaitGroup
	for i := 0; len(slab) > i + stride; i += 2 * stride {
		wg.Add(1)
		i := i
		go func() {
			h := shaPool.Get().(hash.Hash)
			h.Reset()
			h.Write(slab[i : i+32])
			h.Write(slab[i+stride : 32+i+stride])
			// fmt.Printf("L:%d  left : %X, right : %X\n", layerIdx, slab[i:i+32],  slab[i+stride : 32+i+stride])
			h.Sum(slab[i:i])[31] &= 0x3F // callers expect we will reuse-reduce-recycle
			// fmt.Printf("res :  %X, layer : %d\n", slab[i:i+32], layerIdx)
			//if bytes.Equal(slab[i:i+2], []byte{0x97,0xA1}) {
			//	panic("panisc")
			//}
			shaPool.Put(h)
			wg.Done()
		}()
	}
	wg.Wait()

	//fmt.Printf("result size : %d \n", len(slab))
	target <- slab
}

// PadCommP is experimental, do not use it.
func PadCommP(sourceCommP []byte, sourcePaddedSize, targetPaddedSize uint64) ([]byte, error) {

	if len(sourceCommP) != 32 {
		return nil, xerrors.Errorf("provided commP must be exactly 32 bytes long, got %d bytes instead", len(sourceCommP))
	}
	if bits.OnesCount64(sourcePaddedSize) != 1 {
		return nil, xerrors.Errorf("source padded size %d is not a power of 2", sourcePaddedSize)
	}
	if bits.OnesCount64(targetPaddedSize) != 1 {
		return nil, xerrors.Errorf("target padded size %d is not a power of 2", targetPaddedSize)
	}
	if sourcePaddedSize > targetPaddedSize {
		return nil, xerrors.Errorf("source padded size %d larger than target padded size %d", sourcePaddedSize, targetPaddedSize)
	}
	if sourcePaddedSize < 128 {
		return nil, xerrors.Errorf("source padded size %d smaller than the minimum of 128 bytes", sourcePaddedSize)
	}
	if targetPaddedSize > 1<<(MaxLayers+5) {
		return nil, xerrors.Errorf("target padded size %d larger than Filecoin maximum of %d bytes", targetPaddedSize, 1<<(MaxLayers+5))
	}

	// noop
	if sourcePaddedSize == targetPaddedSize {
		return sourceCommP, nil
	}

	out := make([]byte, 32)
	copy(out, sourceCommP)

	s := bits.TrailingZeros64(sourcePaddedSize)
	t := bits.TrailingZeros64(targetPaddedSize)

	h := shaPool.Get().(hash.Hash)
	for ; s < t; s++ {
		h.Reset()
		h.Write(out)
		h.Write(stackedNulPadding[s-5]) // account for 32byte chunks + off-by-one padding tower offset
		out = h.Sum(out[:0])
		out[31] &= 0x3F
	}
	shaPool.Put(h)

	return out, nil
}
