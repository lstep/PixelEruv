/// <reference path="../pb_data/types.d.ts" />
migrate((app) => {
  const collection = app.findCollectionByNameOrId("players");
  collection.fields.add(new TextField({
    name: "sprite_base",
    required: false,
  }));
  return app.save(collection);
}, (app) => {
  const collection = app.findCollectionByNameOrId("players");
  collection.fields.removeByName("sprite_base");
  return app.save(collection);
});
