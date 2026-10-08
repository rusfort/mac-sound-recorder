#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#include <stdlib.h>
#include "capture.h"

extern void goOnAudio(int src, float *l, float *r, int frames);

@interface Rec : NSObject <SCStreamOutput, SCStreamDelegate>
@end

@implementation Rec
- (void)stream:(SCStream *)s didOutputSampleBuffer:(CMSampleBufferRef)sb
        ofType:(SCStreamOutputType)type {
    int src;
    if (type == SCStreamOutputTypeAudio) {
        src = 0;
    } else {
        if (@available(macOS 15.0, *)) {
            if (type != SCStreamOutputTypeMicrophone) return;
            src = 1;
        } else return;
    }
    if (!CMSampleBufferDataIsReady(sb)) return;

    CMFormatDescriptionRef fd = CMSampleBufferGetFormatDescription(sb);
    const AudioStreamBasicDescription *asbd =
        CMAudioFormatDescriptionGetStreamBasicDescription(fd);
    if (!asbd || !(asbd->mFormatFlags & kAudioFormatFlagIsFloat) ||
        asbd->mBitsPerChannel != 32) return;

    CMItemCount frames = CMSampleBufferGetNumSamples(sb);
    size_t sz = 0;
    CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
        sb, &sz, NULL, 0, NULL, NULL, 0, NULL);
    AudioBufferList *abl = malloc(sz);
    CMBlockBufferRef bb = NULL;
    OSStatus st = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
        sb, NULL, abl, sz, NULL, NULL,
        kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment, &bb);

    if (st == noErr) {
        UInt32 ch = asbd->mChannelsPerFrame;
        float *L, *R, *tmp = NULL;
        if (asbd->mFormatFlags & kAudioFormatFlagIsNonInterleaved) {
            L = abl->mBuffers[0].mData;
            R = abl->mNumberBuffers > 1 ? abl->mBuffers[1].mData : L;
        } else {
            float *d = abl->mBuffers[0].mData;
            tmp = malloc(sizeof(float) * frames * 2);
            L = tmp; R = tmp + frames;
            for (CMItemCount i = 0; i < frames; i++) {
                L[i] = d[i * ch];
                R[i] = ch > 1 ? d[i * ch + 1] : d[i * ch];
            }
        }
        goOnAudio(src, L, R, (int)frames);
        free(tmp);
    }
    if (bb) CFRelease(bb);
    free(abl);
}

- (void)stream:(SCStream *)s didStopWithError:(NSError *)e {
    fprintf(stderr, "stream stopped: %s\n", e.localizedDescription.UTF8String);
}
@end

static SCStream *gStream;
static Rec *gRec;

static int fail(char **errOut, NSString *msg) {
    *errOut = strdup(msg.UTF8String);
    return -1;
}

int sc_start(int withMic, char **errOut) {
    __block SCShareableContent *content = nil;
    __block NSError *err = nil;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);

    [SCShareableContent getShareableContentWithCompletionHandler:
        ^(SCShareableContent *c, NSError *e) { content = c; err = e;
                                               dispatch_semaphore_signal(sem); }];
    dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
    if (err) return fail(errOut, err.localizedDescription);
    if (content.displays.count == 0) return fail(errOut, @"no displays");

    SCContentFilter *filter =
        [[SCContentFilter alloc] initWithDisplay:content.displays[0]
                                excludingWindows:@[]];
    SCStreamConfiguration *cfg = [SCStreamConfiguration new];
    cfg.capturesAudio = YES;
    cfg.excludesCurrentProcessAudio = YES;
    cfg.sampleRate = 48000;
    cfg.channelCount = 2;
    cfg.width = 2; cfg.height = 2;                 // video is unused
    cfg.minimumFrameInterval = CMTimeMake(1, 1);

    BOOL mic = NO;
    if (withMic) {
        if (@available(macOS 15.0, *)) { cfg.captureMicrophone = YES; mic = YES; }
        else fprintf(stderr, "microphone capture requires macOS 15+, skipping\n");
    }

    gRec = [Rec new];
    gStream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:gRec];
    dispatch_queue_t q = dispatch_queue_create("callrec.audio", DISPATCH_QUEUE_SERIAL);

    if (![gStream addStreamOutput:gRec type:SCStreamOutputTypeAudio
               sampleHandlerQueue:q error:&err])
        return fail(errOut, err.localizedDescription);
    if (mic) {
        if (@available(macOS 15.0, *)) {
            if (![gStream addStreamOutput:gRec type:SCStreamOutputTypeMicrophone
                       sampleHandlerQueue:q error:&err])
                return fail(errOut, err.localizedDescription);
        }
    }

    [gStream startCaptureWithCompletionHandler:^(NSError *e) {
        err = e; dispatch_semaphore_signal(sem); }];
    dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
    if (err) return fail(errOut, err.localizedDescription);
    return 0;
}

void sc_stop(void) {
    if (!gStream) return;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    [gStream stopCaptureWithCompletionHandler:^(NSError *e) {
        dispatch_semaphore_signal(sem); }];
    dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
    gStream = nil;
}
