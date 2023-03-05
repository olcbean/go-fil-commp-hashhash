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
	mu sync.Mutex
}
type state struct {
	quadsEnqueued uint64
	layerQueues   [MaxLayers + 2]chan []byte // one extra layer for the initial leaves, one more for the dummy never-to-use channel
	resultCommP   chan []byte
	carry         []byte
}

var _ hash.Hash = &Calc{} // make sure we are hash.Hash compliant

// MaxLayers is the current maximum height of the rust-fil-proofs proving tree.
const MaxLayers = uint(31) // result of log2( 64 GiB / 32 )

// MaxPieceSize is the current maximum size of the rust-fil-proofs proving tree.
const MaxPieceSize = uint64(1 << (MaxLayers + 5))

const BlockSize = 127

// MaxPiecePayload is the maximum amount of data that one can Write() to the
// Calc object, before needing to derive a Digest(). Constrained by the value
// of MaxLayers.
const MaxPiecePayload = MaxPieceSize / 128 * 127

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
	if len(cp.layerQueues) != 0 {
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

	if uint64(len(cp.carry))+(cp.quadsEnqueued*127) < MinPiecePayload {
		err = xerrors.Errorf(
			"insufficient state accumulated: commP is not defined for inputs shorter than %d bytes, but only %d processed so far",
			MinPiecePayload, len(cp.carry),
		)
		return
	}

	// If any, flush remaining bytes padded up with zeroes
	if len(cp.carry) > 0 {
		input := cp.carry
		cp.carry = cp.carry[:0]
		if mod := len(input) % 127; mod != 0 {
			input = append(input, make([]byte, 127-mod)...)
		}
		for len(input) > 0 {
			input = input[cp.digestLeadingBytes(input):]
		}
	}

	// This is how we signal to the bottom of the stack that we are done
	// which in turn collapses the rest all the way to resultCommP
	close(cp.layerQueues[0])

	paddedPieceSize = cp.quadsEnqueued * 128
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

	if cp.quadsEnqueued*127+uint64(len(cp.carry))+uint64(inputSize) > MaxPiecePayload {
		return 0, xerrors.Errorf(
			"writing %d bytes to the accumulator would overflow the maximum supported unpadded piece size %d",
			len(input), MaxPiecePayload,
		)
	}

	// just starting: initialize internal state, start first background layer-goroutine
	if cp.carry == nil {
		cp.carry = make([]byte, 0, 127<<14)
		cp.resultCommP = make(chan []byte, 1)
		cp.layerQueues[0] = make(chan []byte, layerQueueDepth)
		cp.addLayer(0)
	}

	for len(cp.carry)+len(input) >= cap(cp.carry) {
		input = input[cp.digestLeadingBytes(input):]
	}

	if len(input) > 0 {
		cp.carry = append(cp.carry, input...)
	}

	return inputSize, nil
}

func (cp *Calc) digestLeadingBytes(input []byte) (processedInputBytes int) {

	quadsToAdd := 1 << uint(63-bits.LeadingZeros64(uint64(len(input)+len(cp.carry))/127))

	// FIXME
	carrySz := len(cp.carry)
	input = append(cp.carry, input...)
	cp.carry = cp.carry[:0]

	firstHoldLevel := 1 << uint(bits.TrailingZeros64(cp.quadsEnqueued))
	if firstHoldLevel != 0 && quadsToAdd > firstHoldLevel {
		quadsToAdd = firstHoldLevel
	}

	//outSlab := bufferPool.Get(quadsToAdd * 128)
	outSlab := make([]byte, quadsToAdd*128)

	for j := 0; j < quadsToAdd; j++ {
		// Cycle over four(4) 31-byte groups, leaving 1 byte in between:
		// 31 + 1 + 31 + 1 + 31 + 1 + 31 = 127
		input := input[j*127 : (j+1)*127]
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

	cp.layerQueues[0] <- outSlab
	cp.quadsEnqueued += uint64(quadsToAdd)
	return quadsToAdd*127 - carrySz
}

func (cp *Calc) addLayer(myIdx uint) {
	// the next layer channel, which we might *not* use
	if cp.layerQueues[myIdx+1] != nil {
		panic("addLayer called more than once with identical idx argument")
	}
	cp.layerQueues[myIdx+1] = make(chan []byte, layerQueueDepth)

	go func() {
		chunkHold := make([]byte, 0, 32)

		for {
			slab, queueIsOpen := <-cp.layerQueues[myIdx]
			// the dream is collapsing
			if !queueIsOpen {

				// I am last
				if myIdx == MaxLayers || cp.layerQueues[myIdx+2] == nil {
					cp.resultCommP <- chunkHold
					return
				}

				if len(chunkHold) != 0 {
					cp.hash254(chunkHold, stackedNulPadding[myIdx], chunkHold)
					cp.layerQueues[myIdx+1] <- chunkHold
				}

				// signal the next in line that they are done too
				close(cp.layerQueues[myIdx+1])
				return
			}

			if len(chunkHold) != 0 {
				if cp.layerQueues[myIdx+2] == nil {
					cp.addLayer(myIdx + 1)
				}
				cp.hash254(chunkHold, slab, chunkHold)
				cp.layerQueues[myIdx+1] <- chunkHold
				chunkHold = chunkHold[:0]
			} else if len(slab) == 32 {
				chunkHold = append(chunkHold, slab...)
			} else {
				if cp.layerQueues[myIdx+2] == nil {
					cp.addLayer(myIdx + 1)
				}

				//				nextOut := bufferPool.Get(len(slab) / 2)

				h := shaPool.Get().(hash.Hash)
				for i := 0; len(slab) > i+32; i += 2 * 32 {
					h.Reset()
					h.Write(slab[i : i+64])
					h.Sum(slab[i/2 : i/2][:0])[31] &= 0x3F // callers expect we will reuse-reduce-recycle
				}
				shaPool.Put(h)

				cp.layerQueues[myIdx+1] <- slab[:len(slab)/2]
				//bufferPool.Put(slab)
			}
		}
	}()
}

func (cp *Calc) hash254(left, right, out []byte) {
	h := shaPool.Get().(hash.Hash)
	h.Reset()
	h.Write(left)
	h.Write(right)
	h.Sum(out[:0])[31] &= 0x3F // callers expect we will reuse-reduce-recycle
	shaPool.Put(h)
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
	if targetPaddedSize > MaxPieceSize {
		return nil, xerrors.Errorf("target padded size %d larger than Filecoin maximum of %d bytes", targetPaddedSize, MaxPieceSize)
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
