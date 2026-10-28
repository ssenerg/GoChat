package io.github.ssenerg.gochat.core

import io.github.ssenerg.gochat.core.media.OpusCaf
import kotlinx.cinterop.BetaInteropApi
import kotlinx.cinterop.ExperimentalForeignApi
import kotlinx.cinterop.addressOf
import kotlinx.cinterop.convert
import kotlinx.cinterop.get
import kotlinx.cinterop.usePinned
import platform.AVFAudio.AVAudioFile
import platform.AVFAudio.AVAudioPCMBuffer
import platform.AVFAudio.AVAudioPlayer
import platform.Foundation.NSData
import platform.Foundation.NSTemporaryDirectory
import platform.Foundation.NSURL
import platform.Foundation.create
import platform.Foundation.writeToFile
import kotlin.math.abs
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/** Apple's own players take what OpusCaf makes of Chrome's recordings. */
@OptIn(ExperimentalForeignApi::class, BetaInteropApi::class)
class OpusCafPlaysTest {
    @Test
    fun avFoundationDecodesChromeRecordings() {
        decodes(Recordings.chromeMono, channels = 1, frames = 21L * 2880)
        decodes(Recordings.chromeStereo, channels = 2, frames = 6L * 2880)
    }

    private fun decodes(webm: ByteArray, channels: Int, frames: Long) {
        val caf = assertNotNull(OpusCaf.rewrap(webm))
        val data = caf.usePinned { NSData.create(bytes = it.addressOf(0), length = caf.size.convert()) }

        val player = assertNotNull(AVAudioPlayer(data = data, error = null))
        assertEquals(frames / 48000.0, player.duration, 0.001)

        val path = NSTemporaryDirectory() + "opus-caf-$channels.caf"
        assertTrue(data.writeToFile(path, atomically = true))
        val file = assertNotNull(AVAudioFile(forReading = NSURL.fileURLWithPath(path), error = null))
        assertEquals(frames, file.length)
        assertEquals(channels.toUInt(), file.processingFormat.channelCount)
        val buffer = assertNotNull(AVAudioPCMBuffer(pCMFormat = file.processingFormat, frameCapacity = frames.toUInt()))
        assertTrue(file.readIntoBuffer(buffer, error = null))
        assertEquals(frames.toUInt(), buffer.frameLength)
        val samples = assertNotNull(buffer.floatChannelData?.get(0))
        var peak = 0f
        for (i in 0 until frames.toInt()) peak = maxOf(peak, abs(samples[i]))
        assertTrue(peak > 0.1f, "the tone decodes to silence: peak $peak")
    }
}
