/// <reference path="../pb_data/types.d.ts" />
migrate((app) => {
  const collection = new Collection({
    name: "players",
    type: "base",
    fields: [
      {
        name: "oidc_sub",
        type: "text",
        required: true,
        min: 1,
        max: 200,
      },
      {
        name: "entity_id",
        type: "text",
        required: true,
        min: 1,
        max: 100,
      },
      {
        name: "display_name",
        type: "text",
        required: false,
        max: 100,
      },
      {
        name: "pos_x",
        type: "number",
        required: false,
      },
      {
        name: "pos_y",
        type: "number",
        required: false,
      },
    ],
    listRule: "",
    viewRule: "",
    createRule: null,
    updateRule: null,
    deleteRule: null,
  });

  return app.save(collection);
}, (app) => {
  const collection = app.findCollectionByNameOrId("players");
  return app.delete(collection);
});
