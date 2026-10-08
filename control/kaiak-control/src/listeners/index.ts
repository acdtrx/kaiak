// The listeners to one kind of change: every module that tells subscribers of changes
// keeps them here, so the rules below hold for each the same way.

export interface Listeners<T> {
  // Subscribes listener; returns the unsubscribe, which is safe to call more than once.
  add(listener: (value: T) => void): () => void;
  // Calls every listener subscribed when the emit begins — one subscribed or
  // unsubscribed meanwhile changes nothing of this emit — each whatever another one
  // does: a listener that throws goes to onError, and the next is called.
  emit(value: T): void;
}

export function createListeners<T>(onError: (error: unknown, value: T) => void): Listeners<T> {
  const listeners = new Set<(value: T) => void>();
  return {
    add(listener) {
      // A wrapper, so the same function subscribed twice is two subscriptions.
      const subscription = (value: T): void => listener(value);
      listeners.add(subscription);
      return () => {
        listeners.delete(subscription);
      };
    },
    emit(value) {
      for (const listener of [...listeners]) {
        try {
          listener(value);
        } catch (error) {
          onError(error, value);
        }
      }
    },
  };
}
