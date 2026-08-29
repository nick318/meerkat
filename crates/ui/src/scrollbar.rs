//! An overlay scrollbar.
//!
//! GPUI ships scroll containers but no scrollbar, and Zed's own is far
//! more than this app needs. This one paints a thumb over the content and
//! drags it: the mouse handlers are registered on the window, not on a
//! hitbox, so a drag keeps working after the pointer leaves the 10px bar.
//!
//! It takes a plain `ScrollHandle`, so it serves the results grid and the
//! sidebar's `uniform_list` alike — the list's handle carries one inside.

use gpui::{
    App, Bounds, Corners, Edges, Element, ElementId, GlobalElementId, Hitbox, HitboxBehavior, Hsla,
    IntoElement, LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, Pixels,
    Point, ScrollHandle, Style, Window, point, px, quad, relative, size,
};
use std::cell::Cell;
use std::rc::Rc;

/// Thickness of the bar, and of the thumb inside it.
pub const THICKNESS: f32 = 10.;
const THUMB_THICKNESS: f32 = 6.;
/// A thumb shorter than this is hard to grab, however long the content.
const MIN_THUMB: f32 = 24.;

/// Where a drag started: which axis, and how far into the thumb the
/// pointer grabbed it, so the thumb does not jump under the cursor.
#[derive(Clone, Copy)]
pub struct Drag {
    vertical: bool,
    grab: Pixels,
}

/// Shared between the two bars of one grid, so only one can be dragging.
pub type DragState = Rc<Cell<Option<Drag>>>;

pub struct Scrollbar {
    vertical: bool,
    handle: ScrollHandle,
    drag: DragState,
    thumb: Hsla,
    thumb_active: Hsla,
}

impl Scrollbar {
    /// `None` when the content fits, so the bar appears only when it can
    /// actually do something.
    pub fn new(
        vertical: bool,
        handle: ScrollHandle,
        drag: DragState,
        thumb: Hsla,
        thumb_active: Hsla,
    ) -> Option<Self> {
        let max = along(handle.max_offset(), vertical);
        (max > px(0.)).then_some(Self {
            vertical,
            handle,
            drag,
            thumb,
            thumb_active,
        })
    }
}

fn along(point: Point<Pixels>, vertical: bool) -> Pixels {
    if vertical { point.y } else { point.x }
}

fn along_size(size: gpui::Size<Pixels>, vertical: bool) -> Pixels {
    if vertical { size.height } else { size.width }
}

/// Where the thumb sits inside a bar of `track` pixels.
fn thumb_span(offset: Pixels, max: Pixels, viewport: Pixels, track: Pixels) -> (Pixels, Pixels) {
    let content = viewport + max;
    let length = if content > px(0.) {
        (track * (viewport / content)).max(px(MIN_THUMB)).min(track)
    } else {
        track
    };
    let progress = (offset / max).clamp(0., 1.);
    ((track - length) * progress, length)
}

/// The scroll offset that puts the thumb's start at `position`.
fn offset_for_thumb(start: Pixels, max: Pixels, track: Pixels, length: Pixels) -> Pixels {
    let travel = track - length;
    if travel <= px(0.) {
        return px(0.);
    }
    max * (start / travel).clamp(0., 1.)
}

pub struct PrepaintState {
    hitbox: Hitbox,
    thumb: Bounds<Pixels>,
    /// Pixels the content can still travel along this axis.
    max: Pixels,
    track: Pixels,
    length: Pixels,
}

impl IntoElement for Scrollbar {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for Scrollbar {
    type RequestLayoutState = ();
    type PrepaintState = PrepaintState;

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, Self::RequestLayoutState) {
        // The parent positions the bar absolutely; fill whatever it gives.
        let mut style = Style::default();
        style.size.width = relative(1.).into();
        style.size.height = relative(1.).into();
        (window.request_layout(style, [], cx), ())
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut Self::RequestLayoutState,
        window: &mut Window,
        _: &mut App,
    ) -> Self::PrepaintState {
        let vertical = self.vertical;
        let viewport = along_size(self.handle.bounds().size, vertical);
        let max = along(self.handle.max_offset(), vertical);
        // The handle's offset runs negative as the content moves up or
        // left; the thumb reads it as distance travelled.
        let offset = -along(self.handle.offset(), vertical);
        let track = along_size(bounds.size, vertical);
        let (start, length) = thumb_span(offset, max, viewport, track);

        let inset = px((THICKNESS - THUMB_THICKNESS) / 2.);
        let thumb = if vertical {
            Bounds::new(
                point(bounds.left() + inset, bounds.top() + start),
                size(px(THUMB_THICKNESS), length),
            )
        } else {
            Bounds::new(
                point(bounds.left() + start, bounds.top() + inset),
                size(length, px(THUMB_THICKNESS)),
            )
        };

        PrepaintState {
            hitbox: window.insert_hitbox(bounds, HitboxBehavior::Normal),
            thumb,
            max,
            track,
            length,
        }
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut Self::RequestLayoutState,
        prepaint: &mut Self::PrepaintState,
        window: &mut Window,
        _: &mut App,
    ) {
        let vertical = self.vertical;
        let dragging = self
            .drag
            .get()
            .is_some_and(|drag| drag.vertical == vertical);
        let active = dragging || prepaint.hitbox.is_hovered(window);

        window.paint_quad(quad(
            prepaint.thumb,
            Corners::all(px(THUMB_THICKNESS / 2.)),
            if active {
                self.thumb_active
            } else {
                self.thumb
            },
            Edges::all(px(0.)),
            gpui::transparent_black(),
            gpui::BorderStyle::default(),
        ));

        let view = window.current_view();
        let thumb = prepaint.thumb;
        let (max, track, length) = (prepaint.max, prepaint.track, prepaint.length);
        let origin = if vertical {
            bounds.top()
        } else {
            bounds.left()
        };

        window.on_mouse_event({
            let (handle, drag, hitbox) = (
                self.handle.clone(),
                self.drag.clone(),
                prepaint.hitbox.clone(),
            );
            move |event: &MouseDownEvent, phase, window, cx| {
                if !phase.bubble() || event.button != MouseButton::Left {
                    return;
                }
                if !hitbox.is_hovered(window) {
                    return;
                }
                let position = along(event.position, vertical);
                if thumb.contains(&event.position) {
                    drag.set(Some(Drag {
                        vertical,
                        grab: position - along(thumb.origin, vertical),
                    }));
                } else {
                    // Clicking the track jumps the thumb to the pointer,
                    // centred, and starts a drag from there.
                    let start = position - origin - length / 2.;
                    set_offset(
                        &handle,
                        vertical,
                        offset_for_thumb(start, max, track, length),
                    );
                    drag.set(Some(Drag {
                        vertical,
                        grab: length / 2.,
                    }));
                    cx.notify(view);
                }
                cx.stop_propagation();
            }
        });

        window.on_mouse_event({
            let (handle, drag) = (self.handle.clone(), self.drag.clone());
            move |event: &MouseMoveEvent, phase, _window, cx| {
                if !phase.bubble() {
                    return;
                }
                let Some(current) = drag.get().filter(|drag| drag.vertical == vertical) else {
                    return;
                };
                let start = along(event.position, vertical) - origin - current.grab;
                set_offset(
                    &handle,
                    vertical,
                    offset_for_thumb(start, max, track, length),
                );
                cx.notify(view);
                cx.stop_propagation();
            }
        });

        window.on_mouse_event({
            let drag = self.drag.clone();
            move |_: &MouseUpEvent, phase, _window, cx| {
                if phase.bubble() && drag.get().is_some_and(|drag| drag.vertical == vertical) {
                    drag.set(None);
                    cx.notify(view);
                }
            }
        });
    }
}

fn set_offset(handle: &ScrollHandle, vertical: bool, travelled: Pixels) {
    let mut offset = handle.offset();
    if vertical {
        offset.y = -travelled;
    } else {
        offset.x = -travelled;
    }
    handle.set_offset(offset);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_thumb_shrinks_as_the_content_grows() {
        // Twice the viewport: half the track, at the top.
        let (start, length) = thumb_span(px(0.), px(100.), px(100.), px(100.));
        assert_eq!(length, px(50.));
        assert_eq!(start, px(0.));

        // Scrolled to the end: the thumb sits against the far edge.
        let (start, _) = thumb_span(px(100.), px(100.), px(100.), px(100.));
        assert_eq!(start, px(50.));

        // Halfway.
        let (start, _) = thumb_span(px(50.), px(100.), px(100.), px(100.));
        assert_eq!(start, px(25.));
    }

    #[test]
    fn a_long_result_still_has_a_grabbable_thumb() {
        // 500 rows of 28px in a 300px viewport.
        let (_, length) = thumb_span(px(0.), px(13_700.), px(300.), px(300.));
        assert_eq!(length, px(MIN_THUMB));
    }

    #[test]
    fn dragging_the_thumb_maps_back_to_the_offset() {
        // Track 100, thumb 50: 50px of travel covers 100px of content.
        assert_eq!(
            offset_for_thumb(px(0.), px(100.), px(100.), px(50.)),
            px(0.)
        );
        assert_eq!(
            offset_for_thumb(px(25.), px(100.), px(100.), px(50.)),
            px(50.)
        );
        assert_eq!(
            offset_for_thumb(px(50.), px(100.), px(100.), px(50.)),
            px(100.)
        );
        // Past either end clamps rather than running off.
        assert_eq!(
            offset_for_thumb(px(-40.), px(100.), px(100.), px(50.)),
            px(0.)
        );
        assert_eq!(
            offset_for_thumb(px(999.), px(100.), px(100.), px(50.)),
            px(100.)
        );
    }

    #[test]
    fn a_thumb_that_fills_the_track_cannot_move() {
        assert_eq!(
            offset_for_thumb(px(10.), px(100.), px(50.), px(50.)),
            px(0.)
        );
    }
}
