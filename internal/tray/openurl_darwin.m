#import <Cocoa/Cocoa.h>
#include "_cgo_export.h"

// BranchdamURLHandler receives the kInternetEventClass/kAEGetURL Apple Event
// LaunchServices sends for a branchdam:// link. macOS never puts the URL in
// argv, and fyne.io/systray's app delegate does not handle it.
@interface BranchdamURLHandler : NSObject
- (void)handleGetURLEvent:(NSAppleEventDescriptor *)event
           withReplyEvent:(NSAppleEventDescriptor *)replyEvent;
@end

@implementation BranchdamURLHandler
- (void)handleGetURLEvent:(NSAppleEventDescriptor *)event
           withReplyEvent:(NSAppleEventDescriptor *)replyEvent {
  NSString *url = [[event paramDescriptorForKeyword:keyDirectObject] stringValue];
  if (url == nil) {
    return;
  }
  branchdamHandleOpenURL((char *)[url UTF8String]);
}
@end

static BranchdamURLHandler *branchdamURLHandler;

void branchdamRegisterOpenURLHandler(void) {
  if (branchdamURLHandler != nil) {
    return;
  }
  branchdamURLHandler = [[BranchdamURLHandler alloc] init];
  [[NSAppleEventManager sharedAppleEventManager]
      setEventHandler:branchdamURLHandler
          andSelector:@selector(handleGetURLEvent:withReplyEvent:)
        forEventClass:kInternetEventClass
           andEventID:kAEGetURL];
}
