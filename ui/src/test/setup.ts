import "@testing-library/jest-dom/vitest";

// jsdom has no layout: scrolling is a no-op there, not an error.
window.scrollTo = () => {};
Element.prototype.scrollIntoView = function () {};
